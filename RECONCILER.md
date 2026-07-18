# 策略發佈平台 — Agent 收斂迴圈規格

> 版本 v0.1 · 搭配 ARCHITECTURE.md 與 PROTOCOL.md 閱讀
>
> 本文件定義 agent 收斂迴圈（reconciler）的實作契約：狀態擁有權、事件序列化、部署狀態機的逐步轉移、進程監督時序。這是把 proto 變成可執行程式碼的最後一層。

---

## 0. 五條不變式（實作時反覆對照）

1. **單一寫者**。所有可變狀態（本地期望狀態副本、每策略實際狀態、進程表）只被 reconciler 那一個 goroutine 讀寫。其他 goroutine（串流收發、部署 worker、健康探測）**只透過 channel 送事件**，永不直接碰狀態。這消滅了進程監督這種充滿共享狀態的程式碼裡的資料競態。
2. **迴圈永不阻塞**。reconcile 迴圈本身只做 O(策略數) 的記憶體內 diff 與狀態轉移，任何 IO（下載、SHA 校驗、drain 等待、健康探測）都丟給 worker goroutine，結果以事件回饋。迴圈卡住 = agent 對進程死亡與新指令雙盲，不可接受。
3. **level-triggered，非 edge-triggered**。每個事件（新 DesiredState、進程退出、tick）都走同一條路徑：更新本地狀態 → 呼叫 `reconcile()` → 計算 desired vs actual 的 diff → 執行收斂動作。沒有「處理某指令」的專屬 handler。
4. **冪等**。`reconcile()` 對同一組 (desired, actual) 呼叫任意多次，結果相同。這讓 resync、重連、重啟後的重放全都安全。
5. **實際狀態來自觀察，不來自記憶**。agent 重啟後不信任任何記憶體假設——實際狀態由掃描 `/opt/strategies/*/current` symlink、讀監督狀態檔、pidfd 重新接管進程重建。desired 來自控制面最新快照。兩者一 diff，收斂繼續。

---

## 1. 事件迴圈骨架

```go
type Reconciler struct {
    desired   map[string]*pb.StrategyAssignmentSpec // strategy -> spec（本地期望副本）
    actual    map[string]*strategyState             // strategy -> 實際狀態
    desiredCh <-chan *pb.DesiredState                // 串流收到新快照
    exitCh    <-chan processExit                     // pidfd 觸發：某進程退出
    workerCh  <-chan workerEvent                     // 部署/drain worker 回報
    outCh     chan<- *pb.AgentMessage                // 送往控制面（status/event/lease）
    clock     Clock
}

func (r *Reconciler) Run(ctx context.Context) {
    tick := r.clock.Ticker(1 * time.Second) // 統一時間輪：健康探測、cron、resync、backoff、lease
    defer tick.Stop()
    for {
        select {
        case ds := <-r.desiredCh:
            r.applyDesired(ds)      // 覆蓋本地期望副本，記錄 generation
        case ex := <-r.exitCh:
            r.handleExit(ex)        // 更新該策略進程狀態、判定 crash/正常退出
        case ev := <-r.workerCh:
            r.applyWorkerEvent(ev)  // 部署階段推進、drain 完成等
        case now := <-tick.C:
            r.tick(now)             // 到期的 cron / 該探測的健康 / 該續的 lease / backoff 到點
        case <-ctx.Done():
            r.shutdown()            // 優雅停機（見 §7）
            return
        }
        r.reconcile()               // 唯一的收斂入口，每個事件後都跑一次
        r.reportStatusIfChanged()   // status 有變才送 StatusReport（去抖）
    }
}
```

> 為什麼所有事件後都無條件 `reconcile()`：因為任一事件都可能改變 desired 或 actual，而收斂決策只依賴這兩者的當前值，不依賴「剛剛發生什麼」。這正是 level-triggered 的體現——事件只是「有東西變了，重算一次」的觸發器，不攜帶動作語義。

---

## 2. 每策略實際狀態

```go
type strategyState struct {
    strategy       string
    phase          pb.DeployPhase   // 部署狀態機當前階段
    runningVersion *pb.ArtifactRef  // 實際跑的版本（可能滯後於 desired）
    runningConfig  *pb.ArtifactRef
    proc           *process         // nil 表示無進程在跑
    inflight       *deployOp        // 非 nil 表示有部署 worker 進行中；擋並發部署
    conditions     map[string]*pb.Condition // Live / Ready / BusinessHealthy
    restartCount   int32
    backoff        backoffState     // 崩潰退避（見 §6.3）
    lease          *leaseState      // 若 spec.lease.required
    lastBadVersion string           // 自動回滾標記的壞版本，收斂時跳過
    observedGen    int64            // 此狀態反映的 spec generation
}

type process struct {
    pid       int
    startTime uint64   // /proc/<pid>/stat 的 starttime，接管時比對防 pid 重用
    pidfd     int      // Linux pidfd，可 poll 的退出通知
    startedAt time.Time
    pgid      int      // setsid 後的 process group，用於群體信號
}
```

---

## 3. reconcile()：diff 與動作決策

```go
func (r *Reconciler) reconcile() {
    // 3.1 對 desired 中每個策略：驅動它朝目標前進
    for name, spec := range r.desired {
        st := r.actual[name]
        if st == nil {
            st = r.initState(name)
            r.actual[name] = st
        }
        r.reconcileOne(spec, st)
    }
    // 3.2 對 actual 中有、但 desired 中沒有的策略：下線
    for name, st := range r.actual {
        if _, want := r.desired[name]; !want {
            r.retireStrategy(st)
        }
    }
}

func (r *Reconciler) reconcileOne(spec *pb.StrategyAssignmentSpec, st *strategyState) {
    // 已有部署進行中 → 不介入，等 worker 事件推進
    if st.inflight != nil {
        return
    }
    // backoff 未到點 → 等 tick 喚醒
    if st.backoff.blockedUntil.After(r.clock.Now()) {
        return
    }
    switch {
    // 目標版本 == 實際版本，且進程健康 → 穩態，什麼都不做
    case versionMatches(spec, st) && st.phase == pb.DeployPhase_HEALTHY:
        return

    // 目標版本 == 實際版本，但進程不在（崩潰後）→ 直接重啟，非重新部署
    case versionMatches(spec, st) && st.proc == nil:
        r.startProcess(spec, st)

    // 目標版本 != 實際版本 → 啟動部署狀態機（跳過已知壞版本）
    case !versionMatches(spec, st):
        if spec.Artifact.Version == st.lastBadVersion {
            r.emitEvent(st, "SkipBadVersion", pb.EventSeverity_WARNING)
            return
        }
        r.beginDeploy(spec, st)
    }
}
```

`versionMatches` 比對 `spec.artifact.digest` 與 `st.runningVersion.digest`（含 config digest）。用 digest 而非 version 字串——內容定址是唯一可信的相等判準。

---

## 4. 部署狀態機

部署是多步、含 IO 的長流程，因此**在 worker goroutine 執行**，每步完成送 `workerEvent` 回主迴圈推進 `phase`。主迴圈只記錄階段，不執行 IO。

### 4.1 階段轉移表

| phase | 前置條件 | 動作（在 worker） | 成功 → | 失敗 → |
|---|---|---|---|---|
| `PENDING` | 有新版本、無壞版本標記 | 建立 `deployOp`，登記 inflight | `DOWNLOADING` | — |
| `DOWNLOADING` | — | 從制品庫拉 artifact 到 `releases/<v>/`（binary tarball 或 OCI pull by digest） | `VERIFYING` | `FAILED` |
| `VERIFYING` | 已下載 | 校驗 SHA256 / 比對 digest | `DRAINING` | `FAILED` |
| `DRAINING` | 已驗證、有舊進程 | 對舊進程送撤單/平倉 hook（SIGTERM + drain endpoint），等優雅退出（上限 `stop_grace_seconds`） | `SWITCHING` | `SWITCHING`（逾時強殺後續進） |
| `SWITCHING` | 舊進程已停 | 原子改 `current` symlink → `releases/<v>`（`renameat2` 或 symlink+rename） | `STARTING` | `FAILED` |
| `STARTING` | symlink 已切 | 若 `lease.required` 先取 lease（§8）；fork/exec 新進程（§6） | `HEALTH_CHECKING` | `ROLLING_BACK` |
| `HEALTH_CHECKING` | 進程已起 | 觀察窗內輪詢三層健康（§9）；監看崩潰次數 | `HEALTHY` | `ROLLING_BACK` |
| `HEALTHY` | 健康窗通過 | 清 inflight，回報最終 status | 穩態 | — |
| `ROLLING_BACK` | 自動回滾觸發 | symlink 切回上一版、重啟、標記 `lastBadVersion` | `ROLLED_BACK` | `FAILED`（連舊版都起不來） |
| `FAILED` | 任一不可恢復錯誤 | 清 inflight，回報錯誤，等下次 desired 變更或人工介入 | — | — |

### 4.2 worker 與主迴圈的介面

```go
type deployOp struct {
    target   *pb.ArtifactRef
    cancel   context.CancelFunc
}

type workerEvent struct {
    strategy string
    phase    pb.DeployPhase   // 推進到的新階段
    err      error            // 非 nil → 走失敗轉移
    proc     *process         // STARTING 成功時帶回新進程 handle
}

// worker 內：每完成一步送一個事件，主迴圈序列化套用
func (w *deployWorker) run(ctx context.Context) {
    send := func(p pb.DeployPhase, err error, proc *process) {
        select {
        case w.out <- workerEvent{w.strategy, p, err, proc}:
        case <-ctx.Done():
        }
    }
    if err := w.download(ctx); err != nil { send(pb.DeployPhase_FAILED, err, nil); return }
    send(pb.DeployPhase_VERIFYING, nil, nil)
    if err := w.verify(); err != nil { send(pb.DeployPhase_FAILED, err, nil); return }
    send(pb.DeployPhase_DRAINING, nil, nil)
    w.drain(ctx) // 逾時不算失敗，強殺後續進
    send(pb.DeployPhase_SWITCHING, nil, nil)
    if err := w.switchSymlink(); err != nil { send(pb.DeployPhase_FAILED, err, nil); return }
    proc, err := w.startProcess(ctx)
    if err != nil { send(pb.DeployPhase_ROLLING_BACK, err, nil); return }
    send(pb.DeployPhase_HEALTH_CHECKING, nil, proc)
    // 健康檢查由主迴圈的 tick 驅動，不在 worker 內輪詢——見 §9
}
```

> 關鍵切分：`HEALTH_CHECKING` 之後的觀察窗**不在 worker 內 sleep 輪詢**，而是交還主迴圈用統一時間輪判定。原因是健康窗期間進程可能崩潰（`exitCh` 事件）、也可能收到新 desired（撤回這次部署），這些都需要主迴圈的全局視野。worker 的職責到「把進程拉起來」為止。

---

## 5. 下線與 drain

```go
func (r *Reconciler) retireStrategy(st *strategyState) {
    if st.proc == nil { delete(r.actual, st.strategy); return }
    if st.inflight != nil { st.inflight.cancel() } // 撤回進行中的部署
    r.spawnDrain(st) // worker：撤單 hook → SIGTERM → 等 grace → SIGKILL → 釋放 lease
}
```

下線與部署的 DRAINING 共用同一套優雅停機時序（§7），差別只在下線後不再拉起新進程、且要釋放 lease。

---

## 6. 進程監督

### 6.1 fork/exec 的三個必要動作

```go
func (r *Reconciler) startProcess(spec *pb.StrategyAssignmentSpec, st *strategyState) (*process, error) {
    cmd := exec.Command(currentBinaryPath(spec), spec.Args...)
    cmd.Env = buildEnv(spec.Env)
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Setsid: true, // ① 獨立 session/process group：agent 退出不連帶終止策略（自我更新前提）
    }
    // ② 進 cgroup v2：exec driver 下寫 cgroup.procs 施加 CPU/記憶體/FD 限制
    cmd.SysProcAttr.UseCgroupFD = true
    cmd.SysProcAttr.CgroupFD = r.cgroupFor(st, spec.Limits)

    if err := cmd.Start(); err != nil { return nil, err }

    pid := cmd.Process.Pid
    // ③ pidfd：拿可 poll 的退出通知，交給監督 goroutine
    pidfd, _ := unix.PidfdOpen(pid, 0)
    startTime := readProcStartTime(pid) // 接管時比對，防 pid 重用
    go r.watchExit(st.strategy, pid, pidfd, startTime)
    return &process{pid: pid, pidfd: pidfd, startTime: startTime, startedAt: r.clock.Now(), pgid: pid}, nil
}
```

### 6.2 退出監督

```go
func (r *Reconciler) watchExit(strategy string, pid, pidfd int, startTime uint64) {
    pollWait(pidfd) // pidfd 可讀 = 進程已退出
    code := reapOrProbe(pid)
    r.exitCh <- processExit{strategy, pid, startTime, code, r.clock.Now()}
}
```

主迴圈的 `handleExit` 收到後：先用 `(pid, startTime)` 確認是當前受管進程而非過時通知，再依「退出時機」判定是正常下線還是崩潰。

### 6.3 崩潰退避與 startsecs

抄 supervisord 的成熟參數化：

```go
type backoffState struct {
    consecutive  int           // 連續崩潰次數
    blockedUntil time.Time     // 在此之前不重啟
}

func (r *Reconciler) handleExit(ex processExit) {
    st := r.actual[ex.strategy]
    if st == nil || st.proc == nil || st.proc.pid != ex.pid || st.proc.startTime != ex.startTime {
        return // 過時通知，忽略
    }
    lived := ex.at.Sub(st.proc.startedAt)
    st.proc = nil

    if r.retiring(st) { // 正在下線 → 正常退出，收工
        delete(r.actual, st.strategy)
        return
    }
    policy := r.desired[st.strategy].DeployPolicy
    if lived < time.Duration(policy.Startsecs)*time.Second {
        // 未活過 startsecs → 視為啟動失敗，計入崩潰
        st.backoff.consecutive++
        st.restartCount++
        // 部署健康窗內崩潰超限 → 自動回滾
        if st.phase == pb.DeployPhase_HEALTH_CHECKING &&
            st.backoff.consecutive > int(policy.MaxCrashesInWindow) &&
            policy.EnableAutoRollback {
            r.beginRollback(st)
            return
        }
        // 否則指數退避後重啟（reconcile 在 tick 到點時執行）
        st.backoff.blockedUntil = r.clock.Now().Add(expBackoff(st.backoff.consecutive))
    } else {
        st.backoff.consecutive = 0 // 活夠久 → 重置退避，正常重啟
    }
    // 不在此處直接重啟——交給 reconcile()，保持單一收斂入口
}

func expBackoff(n int) time.Duration {
    d := time.Second << min(n, 6) // 1,2,4,...,64s 封頂
    return d + jitter(d/4)
}
```

> `startsecs` 的作用：區分「進程正常運行後才退出」與「一起來就崩」。沒有它，快速崩潰迴圈會被誤計為 N 次成功重啟，掩蓋壞版本。

---

## 7. 優雅停機（drain 時序）

部署 DRAINING、策略下線、agent 收到 SIGTERM 三種情況共用：

```
1. 呼叫策略的撤單/平倉 hook（drain endpoint over unix socket），等其回報「已平倉/已撤單」
2. 送 SIGTERM 給 process group（pgid，涵蓋策略 fork 的子進程）
3. 等待，上限 stop_grace_seconds
4. 仍未退出 → 送 SIGKILL
5. 釋放 lease（若持有）
```

agent 自身收到 SIGTERM 時**不殺策略進程**——只停止 reconcile、乾淨關閉串流、持久化監督狀態檔（供自我更新後的新 agent 接管，見 §10）。策略進程因 setsid 獨立存活。這是 agent 更新不影響策略的根基。

---

## 8. Lease（防雙開）整合

```go
func (r *Reconciler) ensureLease(st *strategyState, spec *pb.StrategyAssignmentSpec) leaseOutcome {
    if !spec.Lease.Required { return leaseNotNeeded }
    if st.lease.held && st.lease.expiresAt.After(r.clock.Now().Add(renewMargin)) {
        return leaseHeld
    }
    // 送 LeaseRequest，等 LeaseResponse（經 workerCh 回饋，不阻塞主迴圈）
    r.outCh <- leaseRequestMsg(st.strategy, spec.Lease.TtlSeconds)
    return leasePending // STARTING 階段暫停，直到 lease 到手
}
```

- 續約在 tick 中驅動：`expiresAt - renewMargin` 到點就送 `LeaseRenew`。
- **續約失敗即自殺**：若 lease 過期前未成功續約，reconciler 立即 drain 該策略並送 `Event{reason:"LeaseSuicide"}`。這是網路分區時防止雙開下單的最後防線——寧可停也不可雙跑。

---

## 9. 三層健康檢查整合

健康探測在獨立 goroutine 執行（避免阻塞主迴圈的 IO），結果以事件回饋，主迴圈據此更新 `conditions` 與推進/否決 `HEALTH_CHECKING`。

| condition | 探測方式 | 影響 |
|---|---|---|
| `Live` | pidfd（進程活著） | FALSE → 觸發 §6.3 崩潰路徑 |
| `Ready` | 策略 health endpoint（unix socket）：已連行情/交易所、佇列未積壓 | HEALTH_CHECKING 內須轉 TRUE 才進 HEALTHY |
| `BusinessHealthy` | 策略主動 heartbeat 的業務指標：最後行情時間戳、掛單數、PnL | 穩態下 FALSE（如 30s 無行情）→ 告警，可選觸發重啟 |

`HEALTH_CHECKING → HEALTHY` 的判定：觀察窗內 `Ready` 持續 TRUE 且崩潰次數未超限。窗口由 tick 計時，非 worker sleep。

---

## 10. 自我更新中 reconciler 的角色

agent 版本本身是 `DesiredState.desired_agent_version` 的一部分，因此「該更新 agent」也是一次收斂：

```
1. reconcile 發現 desired_agent_version != 自身版本 → 觸發自我更新 worker
2. 舊 reconciler：持久化監督狀態檔（每策略的 pid + startTime + pgid + runningVersion + phase）
3. 舊 agent 乾淨退出（§7：不碰策略進程）
4. systemd 拉起新 agent → 新 reconciler 啟動時 rebuildActualState()：
     讀狀態檔 → 對每個 pid 用 pidfd_open 重新接管 → 比對 startTime 防 pid 重用
     掃 current symlink 確認 runningVersion
5. 新 reconciler 拿控制面最新 DesiredState → reconcile() → 因實際==期望，穩態，無擾
```

`rebuildActualState()` 是不變式 5 的落地：新 agent 不信任任何記憶，實際狀態全靠觀察（狀態檔 + pidfd + symlink）重建。監督狀態檔自身也需版本化（schema 演進：舊 agent 寫的要能被新 agent 讀懂）。

---

## 11. 失敗處理矩陣

| 失敗 | reconciler 行為 |
|---|---|
| 下載/校驗失敗 | phase→FAILED，保留舊進程運行，等下次 desired 變更 |
| 新版本起不來（未過 startsecs） | 退避重啟；健康窗內超限→自動回滾 |
| 自動回滾後舊版也起不來 | phase→FAILED，送 ERROR 事件，停在無進程狀態等人工 |
| 串流斷線 | 進程與收斂狀態**不受影響**；重連後收全量快照重新 diff |
| lease 續約失敗 | drain 該策略並自殺（防雙開優先於可用性） |
| pidfd 過時通知（pid 重用） | startTime 比對不符 → 忽略 |
| 收到未知指令 | 送 Nack（見 PROTOCOL §9），不執行 |
| agent 自我更新失敗 | systemd guard 腳本 symlink 切回舊版（在 reconciler 之外，見 ARCHITECTURE §11） |

---

## 12. 測試要點

- **收斂冪等性**：對固定 (desired, actual) 連呼 `reconcile()` N 次，動作只發生一次。
- **確定性時鐘**：`Clock` 介面注入假時鐘，退避/健康窗/lease TTL 全可快轉測試，不 sleep 真實時間。
- **崩潰迴圈**：模擬進程一起來就退，驗證 startsecs 判定與指數退避封頂。
- **pid 重用**：構造 (pid 相同, startTime 不同) 的過時退出事件，驗證被忽略。
- **接管**：殺掉 agent（非策略進程），重啟後驗證 rebuildActualState 正確用 pidfd 重新接管、狀態無誤判。
- **分區雙開**：兩個 reconciler 競爭同策略 lease，驗證只有一個啟動、另一個 pending 或自殺。
- **部署撤回**：HEALTH_CHECKING 中途送新 desired，驗證進行中的部署被 cancel 且不留半成品。
```