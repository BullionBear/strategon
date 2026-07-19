# 策略發佈平台 — 改進清單與設計修正

> 版本 v0.2 · 承接 ARCHITECTURE / PROTOCOL / RECONCILER / SAFETY / FRONTEND 五份文件與 `main` 分支實作
>
> v0.2:對照實際代碼校正各項現況(修掉過時/低估的描述),納入 A7(策略自管優雅退出)、B3 定調 Discord OAuth + 扁平 authz,Part C 改為分批(Tranche 0–4);基線更新至 PR #4 修正後。
>
> 本文件不重述設計,只記錄兩類東西:(1) 討論中**修正或澄清**的設計決策(推翻或改寫了原文件的部分);(2) 現有實作之上的**改進 backlog**,按「阻擋上線的嚴重程度」而非功能清單排序。

---

## Part A — 設計修正與澄清

這些是討論過程中改變或講清楚的決策,應回填進對應文件。

### A1. Lease 邏輯內聚於策略進程,agent 完全不碰(推翻 SAFETY 原定調)

**原文件隱含**:agent 代管 lease 續約,並把 deadline 共享給策略做下單前檢查(架構 A)。

**修正為**:lease 的**整個生命週期(取得、續約、下單前檢查)內聚在策略進程**,由策略 SDK 提供;agent 只做進程管家,不參與 lease(架構 B)。

**理由**:策略下單與 agent 無關——agent 不在策略連交易所的資料路徑上。把安全關鍵的下單前檢查橫跨 agent↔策略兩個進程、靠共享通道傳 deadline,會引入「這個 deadline 是否還新鮮」的故障面(agent 掛了策略拿著過期 deadline 裸奔)。內聚在策略進程則:故障模式更少、離下單路徑(唯一能正確執行檢查的地方)最近、agent 回歸純粹管家。策略不用自己懂 lease 協議這個好處,用**策略 SDK / 框架層**解決(提供 lease 客戶端庫,`sdk.check_lease_before_order()`),而非讓 agent 當中間人。

**待辦**:改寫 SAFETY §2/§3/§8 的 lease 歸屬;PROTOCOL 的 lease 訊息(LeaseRequest/Renew/Response)的客戶端從 agent 移到策略 SDK;`LeaseSpec` 保留在 spec 但語義改為「策略 SDK 據此向控制面取 lease」。

### A2. 下單前同步檢查的定義與實作歸屬(澄清 SAFETY §3.2a)

**下單前同步檢查**:策略在**每一次送單前**,當場、同步、內聯地用 monotonic clock 驗證自己仍持有有效 lease,驗不過即拒單自殺。它堵的是進程凍結解凍後、背景續約迴圈尚未察覺 lease 已死之前那個「失明窗口」裡的第一筆單。

**關鍵細節**:
- 用 **monotonic clock** 算 lease deadline(不受 NTP 調整干擾),**同時**比對 monotonic 與 wall-clock 的相對前進,偵測 suspend 造成的跳變;偵測到跳變即自殺重取 lease(§3.2a + §3.2b 縫合)。
- 必須在**策略進程的下單臨界路徑**上,不能只在背景迴圈(那是失明窗口的來源),也不能在 agent(agent 看不到「即將 emit 一筆單」的那一刻)。
- 對策略**有侵入性**:策略框架必須強制每個策略在下單路徑插這道檢查,不能靠策略作者自覺——這是框架層下沉的約束。

**待辦**:策略 SDK 提供 `check_lease_before_order()`;框架強制下單路徑經過它;文件明確標註殘餘風險(§3.3:通過檢查到單送達之間的微秒窗口無法消除,需交易所端 fencing)。

### A3. 三條 HTTP 連線的協議定調(澄清 ARCHITECTURE/PROTOCOL)

討論中釐清「用了 gRPC 為什麼還走 HTTP」的矛盾,結論是**協議跟著客戶端走**,三條連線各自選型,無矛盾:
- Agent↔控制面(南北向):gRPC 雙向串流(機器、高頻、串流)。
- Trader↔控制面(人向):HTTP/JSON via **Connect**(瀏覽器、低頻、需可 curl);用 Connect 讓同一份 proto 同時服務 gRPC/gRPC-Web/HTTP-JSON,避免兩套介面定義。
- Agent↔本地策略(探活):unix socket(同機、最低摩擦)。

**前端即時更新用 Connect server-streaming,不用 WebSocket**:面板流量嚴重不對稱(server 持續推、client 零星送指令),用不到 WebSocket 全雙工;server-streaming 推狀態 + unary 送指令更一致,且不脫離 proto 型別保證。

### A4. Digest 由源頭提供,控制面不得自算(澄清內容定址的信任性質)

**問題**:能否讓控制面自己算 artifact 的 sha,省掉手動?

**結論**:**不能由控制面自算**。digest 是「使用者聲明要部署什麼」的密封標籤,必須由信任鏈源頭(CI)產生,一路不變傳到 agent,agent 執行前重算比對。控制面自算 = 中途重算 = 驗證退化為「agent 下載到的 == 控制面下載到的」,不再證明「這是你當初想部署的版本」,中間人掉包可繞過。

**正解**:CI build 當下在可信環境算 sha 並自動註冊(build/publish 左邊界,見 B7)。過渡期由人手動算(人肉扮演信任源頭)。若為本地 dev 便利,可加**顯式** `compute_digest` 旗標(明確標記「本次放棄源頭驗證」),生產禁用——關鍵是讓放棄驗證成為有意識的選擇而非預設。

### A5. Agent 升級對策略進程的影響(澄清當前實作的真實行為)

**setsid 已實作**,所以:
- **控制面重新發佈** → 策略進程完全不受影響(不在同機、串流斷線不觸發進程操作)。**現在就安全。**
- **agent 退出/升級這個動作本身** → 策略進程不被 kill(setsid 分離)。**已實作。**

**但接管未實作**,所以當前真實行為是:
- 新 agent 啟動從空白狀態收斂,不認識舊 agent 留下的孤兒策略進程 → **重新啟動一份** → 同策略跑兩份(不是被 kill,是相反:重複)。無 lease 時這就是雙開。

**過渡緩解**:接管實作前,agent 升級**不是無擾操作**;安全做法是升級前先透過控制面 drain 該機器策略(改期望狀態移除)、升級後重新 assign。代價是升級期間停機,但不會重複。

### A6. 接管機制入代碼、升級策略歸編排、失敗守護必外置(職責劃分)

「不被 kill / 不重複」的接管該寫代碼還是腳本?答案是按職責切:
- **接管機制 → agent 代碼**(狀態檔寫入 + 啟動時 rebuildActualState + Adopt)。它是**正確性保證**,不能依賴「運維記得跑腳本」;且必須在 `reconcile()` 首次呼叫前完成(關窗是毫秒級時序,腳本無法介入)。
- **升級失敗守護 → systemd guard 腳本**(agent 之外)。壞到起不來的 agent 無法回滾自己,**架構上必須外置**;順帶讓被入侵的 agent 也改不動自己的回滾邏輯。
- **canary 批次節奏 → 控制面/運維編排**。需跨機全局視野,單 agent 無資格也無資訊做批次決策。

### A7. 優雅退出內聚於策略進程,agent 只做「訊號 + 逾時 + 強殺」(推翻 agent 代管 drain)

**原實作隱含**:`supervisor.StopSequence` 帶一個 `Drain` hook——由 **agent** 在 SIGTERM 前呼叫策略的「撤單/平倉 endpoint」(RECONCILER §4/§5 的 "drain endpoint")。

**修正為**:**優雅退出(撤單、平倉、收尾在途單)內聚在策略進程**,由策略在 SIGTERM handler 內自行完成;agent 只送 `SIGTERM`,等 `stop_grace_seconds`,逾時 `SIGKILL`。刪掉 `StopSequence.Drain` hook 與 agent↔策略的 drain socket。

**理由**:與 A1 同源——agent 不在策略連交易所的資料路徑上,沒有資格也沒有連線去撤單。唯一知道「有哪些在途單、如何安全平掉」的地方是策略自己;agent 回歸純管家。逾時強殺是**防阻塞的安全網**:策略 SIGTERM handler 掛住時,不能拖垮部署/下線。

**待辦**:
- 代碼:移除 `StopSequence.Drain`(`deploy.go` 已傳 `nil`,agent 路徑不變:SIGTERM → poll grace → SIGKILL)。
- 文件:RECONCILER §4(DRAINING 由「SIGTERM + drain endpoint」改為「SIGTERM;策略自 drain;逾時 SIGKILL」)、§5 spawnDrain 同步改寫。
- SDK(擴大 B1 範圍):策略 SDK 除 lease 客戶端外,再提供**優雅退出契約**(`on_shutdown()`/SIGTERM handler:撤單 → 平倉 → ack 退出),框架強制,不靠策略作者自覺。

**關鍵約束(與 B1/SAFETY §4 咬合)**:
- `stop_grace_seconds` 必須 **≥ 策略最壞情況 drain 時間**,但 **< lease 安全 margin**。太小 → 強殺打斷 drain;太大 → drain 拖過 lease 過期點,另一台已開始接管。
- **強殺不取代 B1**:被 SIGKILL 打斷的策略可能在交易所留下**活單**,agent 隨即啟動新版本 → 正是要避免的雙重曝險。孤兒活單正是 exchange 端 fencing / lease-suicide 要收的口。故「策略自管優雅退出」**降低**但**不消滅** B1 的必要性。

---

## Part B — 改進 Backlog(按阻擋上線嚴重程度排序)

> 判準:能否安全跑真金白銀策略。「虧錢項」最高優先,其餘按「讓『上線』一詞成立」的必要性排。

> **基線註(2026-07 / PR #4)**:三個原 backlog 默認「已能運作」的正確性缺口已修掉,下列以此基線為準——(1) crash-loop 自動回滾曾**永久卡在 HEALTH_CHECKING**(`reconcileOne` 的 inflight guard 擋掉背景重啟,crash 計數到不了回滾門檻);(2) 退出的策略進程**洩漏成 zombie**(`WatchExit` 未 reap,`processAlive` 又把 zombie 讀成活著);(3) `SkipBadVersion` **每 tick 洗版**。已在原生 arm64 Linux 端對端驗證回滾成立。

### B1.【虧錢項·最高】Fencing lease + 安全硬化(SAFETY.md 整份)

**現狀(MVP 已落地)**:`LeaseService` unary(Acquire/Renew)在 agent port;`store` 租約授權 + `--lease-margin-cp`;Deploy 跨機互鎖;`sdk/lease` 取得/續約/`CheckBeforeOrder`(含粗粒度 clock-jump);`cmd/lease-demo`。Agent 串流 lease 訊息仍 no-op(A1)。PG 持久化 leases 表、NTP/殘餘風險標註、框架強制下單攔截、SAFETY §8 全清單**尚未**完成。

**這是唯一「錯了會直接虧錢」的缺口**,單獨足以否決任何會下單的策略上線。剩餘:
- 框架強制每筆下單必經 `CheckBeforeOrder`(不能靠策略作者自覺)。
- 遷移互鎖完備:unreachable 後自動遷移編排仍須等 lease 過期(手動 Deploy 跨機已擋)。
- 時鐘假設落地:NTP 同步 + 失步告警;margin 依實測設定(SAFETY §5)。
- 誠實標註殘餘風險,按策略能否容忍分流(SAFETY §3.3/§9)。
- 持久化 `leases` 表(現為 memory / PG 進程內 map)。

**驗收**:SAFETY §8 的上線前檢查清單全過。

### B2.【基礎設施】Postgres 持久化(取代 in-memory store)

**現狀**:in-memory store,控制面重啟即丟全部(artifact 註冊、指派、審計)。

**後果**:控制面不能重啟/升級/HA;DR 無從談起(DR 前提是「真相在 PG、控制面無狀態可重建」)。一個重啟丟全部狀態的控制面,定義上不能上線。

**有利條件**:store 已是介面(`store.go` + `memory.go`),替換隔離——reconciler/grpcstream/api 都不知道 store 是什麼。**先做前端已產出真實查詢需求**,現在設計 schema 是需求驅動而非猜測。schema 概念層見 ARCHITECTURE §16.3。

### B3.【安全面】mTLS enrollment(機器)+ Discord OAuth(人)

**現狀**:AgentService 已支援 **offline Ed25519 mTLS**(`strategon-ca` + `--tls-*` / `--client-ca` / `--server-ca`;CN = machine id)。人向 API **仍無認證**,靠綁 `127.0.0.1` + 寬鬆 CORS 擋(`cmd/controlplane/main.go`)。線上 enrollment(token→CSR)與憑證撤銷尚未做。

**拆成正交的軸,可分開上:**
- **人 authN — Discord OAuth(定調)**:瀏覽器走 Discord OAuth flow → session/token → Connect **interceptor** 對每個 unary/streaming 呼叫驗證(unary + streaming 共用)。不碰 agent 路徑,自成一塊,可在被動策略試點時先上。
- **人 authz — 暫定扁平(有意識的取捨)**:**任何通過 Discord 登入者 = 完整 operator 權限**,無 per-user 權限模型。這是明說的姿態而非遺漏(同 A4 的「有意識放棄」)。未定義問題第 10 點(誰能部署到哪台、能否看別人策略)**明確 park**,待多團隊/多租戶需求出現再開。
- **機器 authN — mTLS**:offline CA 已落地;下一步是 PROTOCOL §6 的一次性 token → CSR → 簽發長期憑證,以及撤銷。與人向認證正交,獨立節奏。
- **網路暴露**:控制面對公網隱身,agent 出站;跨區依雲廠商選 VPC peering / overlay(ARCHITECTURE §4)。

### B4.【核心功能·原始需求】Cron 本地執行器

**現狀**:UI 能寫 cron spec 進期望狀態,但 **agent 不執行**。

**這是最初需求**(「每日 00:00 重啟進程」)目前是只進不出的設定框。含:agent 本地 cron 執行(spec 下發、本地執行、結果回報)、顯式時區、多機抖動/分批(ARCHITECTURE §10)。cron 動作與部署狀態機的競態需定義(未定義問題第 3 點:inflight 時 cron 動作延後)。

### B5.【無擾運維】Agent 自我更新接管

**現狀(MVP 已落地)**:`<--base>/agent/supervision.json` 原子持久化;`Run` 開頭 `rebuildActualState` + `Adopt` 再 reconcile——agent 重啟不再重複拉起策略。`processAlive` 仍用 Adopt 防 pid 重用。

**剩餘**:`desired_agent_version` 自我更新 worker、systemd guard 失敗回滾(A6)、canary 批次(RECONCILER §10);狀態檔 schema 演進路徑待第二版。

**有利條件**:PR #4 的 driver 收屍(reap)已刻意只對 `owned`(自己 fork 的)進程動手,跳過 Adopt 的進程(接管後歸 init 收),故接管路徑不會被收屍邏輯回退。

### B6.【持久化/供應鏈】真 artifact registry(S3/MinIO)

**現狀**:artifact 用 `file://`,跨機發布要求檔案已在 agent 端;無內容定址存儲。

**含**:S3/MinIO 存二進位、PG 存 metadata;OCI image driver 的 pull-by-digest(為 Python/ML 策略預留,ARCHITECTURE §14);release 磁碟保留與 GC(未定義問題第 6 點:`releases/` 累積會塞爆磁碟,需保留策略)。

### B7.【左邊界】Build/publish 流程與策略註冊

**現狀**:「策略程式碼 → registry 制品」整段空白;機器註冊很細,策略註冊無對應物(未定義問題第 9 點)。

**含**:CI build → 可信環境算 digest → 自動註冊進控制面(消掉 A4 的手動算 sha,且保住信任性質)。這條做完,「手動算 sha」從人身上移到建置流程。

### B8.【核心功能黑洞】Secrets 管理

**現狀**:完全缺席。交易所 API key/簽名私鑰**不可**走「配置即制品」路徑(config 制品在 S3、可讀、可版本化)。

**含**(SAFETY §7 已立邊界):密鑰不落制品;注入與 config 分離;可獨立輪換不觸發重新部署;讀取受策略進程 user 邊界約束(需 B9 的每策略 user 才真正隔離)。完整生命週期另立 SECRETS.md。

### B9.【隔離強化】策略間資源與身分隔離

**現狀**:所有策略同一非 root user(A5 定調的簡單起點);資源限制靠 cgroup 但策略間憑「約定」。

**升級路徑**(策略密度/來源多樣性上升時):
- 每策略獨立 user(策略間碰不到彼此檔案/密鑰)——代價:pidfd 接管跨 user 需 `CAP_SYS_PTRACE` 或改機制(SAFETY §6.4)。
- 資源隔離從「約定」升為 cgroup v2 強制(ARCHITECTURE §18)。
- 這與 B8 secrets 隔離耦合:每策略 user 是密鑰隔離真正成立的前提。

### B10.【可觀測性】監控控制面自身

**現狀**:監控被管理的機器,但無人監控控制面自己(未定義問題第 11 點)。

**含**:控制面自身指標、每機 reconcile 滯後、「多少機器處於 drifted 狀態」的聚合視圖、串流的背壓與 reconnect storm 防護(未定義問題第 7 點:控制面重啟時 agent 同時重連的速率限制、斷線期間事件持久化語義)。

### B11.【前端補完】歷史/審計頁與艦隊串流

**現狀**:audit/schedules 頁是佔位(in-memory 下為空);艦隊總覽用輪詢。

**含**(依賴前置):audit 頁接 Postgres(依賴 B2);歷史趨勢圖(依賴 B2/TSDB);機器多時加 `WatchFleet` 合併串流(FRONTEND §4.2)。

### B12.【健康檢查深化】三層健康與業務健康

**現狀**:liveness(pidfd)有;**readiness 已半成**——`health.UnixSocketChecker`(unix socket 上 HTTP GET `/healthz`)已實作,但 agent 目前只掛 `health.AlwaysReady{}`(`cmd/agent/main.go`)。業務健康完全缺席。

**拆成:**
- **B12a(便宜)**:agent 從 `AlwaysReady` 換成已實作的 `UnixSocketChecker` + 約定策略端 `/healthz`。基礎設施已在,只差接線與約定——併入 Part C Tranche 0。
- **B12b(較大)**:業務 heartbeat(最後行情時間戳、掛單數、PnL)——「進程活著但 30s 無行情」的半死狀態只有這層抓得到(RECONCILER §9),與 B1 策略 SDK 一併下沉到框架。

---

## Part C — 上線最小必要路徑(依賴排序 + 分批)

> 原則:先讓 happy path **能被運維**(小,以小時~天計),再攻大阻擋項(以週計)。判準仍是「能否安全跑真金白銀」。原本一路 B1→B2→B3 每項都以週計,但一批**小時~天級**的運維前置(散在 B10/B12/#5/#6)才是被動策略試點的真正瓶頸,故前置為 Tranche 0。

**Tranche 0 — 讓 happy path 可運維(小,解鎖「能跑、能除錯」任何策略)**
- **#5 stdout/stderr 捕獲** → 每 release logfile / journald。現況 agent 不接 stdout,Go 預設丟 `/dev/null`,**策略日誌今天等於沒有**。
- **#6 `releases/` 保留與 GC**(保留 N 份)。現況 `SwitchTo` 只換 symlink,舊 release 無限累積會塞爆磁碟。
- **B12a**:agent 從 `AlwaysReady` 換成已實作的 `UnixSocketChecker` + 策略 `/healthz` 約定。
- (PR #4 已補上 crash-loop 回滾 + 收屍的正確性基線,見 Part B 基線註。)

**Tranche 1 — 控制面成為真服務**
- **B2 Postgres**(store 已是介面,替換隔離)——解鎖重啟/DR/HA,以及 **audit/事件持久化**(現況 in-memory,重啟丟全部事件,「昨晚發生什麼」無從回答)。
- **B10a** 重連節流 + 斷線期間事件持久化語義(與 B2 咬合)。

**Tranche 2 — 信任與暴露**
- **B3-人 Discord OAuth**(authN;扁平 authz)——足以把 UI 擺在登入後做**非交易**試點。
- **B3-機** 線上 mTLS enrollment + 撤銷(offline Ed25519 mTLS 已落地)。
- **B7 CI build → 可信環境算 digest → 自動註冊**(真正退掉 A4 的手動算 sha,且保住信任性質)。

**Tranche 3 — 虧錢閘門(任何送真實訂單前,不可跳過)**
- **B1 剩餘** = 框架強制下單攔截、NTP/殘餘風險運維、持久化 leases、自動遷移編排完備 + SDK 優雅退出(A7)。MVP 已有:`LeaseService` + `sdk/lease` + Deploy 跨機互鎖;agent 串流 lease 仍正確 no-op。

**Tranche 4 — 功能補完**
- **B4 cron**(+ 與部署狀態機競態,未定義 #3;對交易策略還與 A7 退出/lease 自殺咬合——先做被動負載,交易負載待競態定義)。
- **B5 剩餘**:自我更新 worker + systemd guard + canary(接管 MVP:狀態檔 + `rebuildActualState` 已落地)。
- **B6 S3/OCI**、**B8 secrets**、**B9 每策略 user**、**B11 fleet UI**。

**折衷上線路徑(不變,但更快)**:純被動、不下單的策略(行情記錄、監控、paper trading)無虧錢風險,B1 不擋。做完 **Tranche 0 + Tranche 1 + B3-人**,即可把這類負載擺上真實環境,驗證收斂/監控/回滾/日誌。**任何送真實訂單的策略必須等 Tranche 3(B1)。**

---

## 附錄:未定義問題對照(討論中盤點,已併入上表)

| # | 問題 | 落點 |
|---|---|---|
| 1 | Fencing 凍結下的有界風險 | B1 / A2 |
| 2 | 時鐘偏差假設未明說 | B1(SAFETY §5) |
| 3 | Cron 與部署狀態機競態 | B4 |
| 4 | Secrets 管理缺席 | B8 |
| 5 | 日誌/stdout 去向(現況:未接線 → `/dev/null`,日誌全丟) | Tranche 0(#5);亦為 B1 LeaseSuicide 除錯依賴 |
| 6 | Release 磁碟保留與 GC | B6 |
| 7 | 串流背壓與 reconnect storm | B10 |
| 8 | 策略指派生命週期與遷移互鎖 | B1(SAFETY §4) |
| 9 | Build/publish 邊界與策略註冊 | B7 |
| 10 | 授權模型 | B3(人 authN = Discord OAuth;authz 扁平、明確 park) |
| 11 | 監控控制面自身 | B10 |
