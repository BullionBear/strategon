# 策略發佈平台 — 架構設計文件

> 版本 v0.1 · 狀態：設計定稿（待實作）

一個讓 trader 註冊自有交易機器、發佈與監控策略進程的內部平台。核心設計哲學是 **level-triggered 期望狀態收斂**（借鑑 kubelet），把發佈、回滾、災難復原統一為同一個機制：改期望狀態，等 agent 收斂。

---

## 1. 設計目標與範圍

### 1.1 功能需求

- Trader 在雲端開好機器後，一行 bootstrap 即可把機器註冊到平台。
- 平台實時監控每台機器的資源（CPU / 記憶體 / 磁碟 / 網路）與各受管進程狀態。
- 透過平台發佈策略；發佈後監控進程健康狀況。
- 支援一鍵回滾與自動回滾。
- 支援災難復原（控制面重建、機器重開、整機蒸發後重新指派）。
- 定時任務：每日 00:00 重啟進程、修改配置等，可自訂 cron。

### 1.2 非功能需求

| 面向 | 目標 |
|---|---|
| 可用性 | 控制面短暫故障不影響已在跑的策略；agent 出站、控制面無狀態可快速重建 |
| 安全 | 控制面對公網隱身；agent 出站連線 + mTLS；機器身分可撤銷 |
| 正確性 | 防止同一策略雙開下單（fencing lease）；每次變更可審計、可回滾 |
| 運維面 | 極簡：兩個 Go 靜態二進位 + PostgreSQL + TSDB |
| 規模 | 單控制面支撐數百台機器；上千台再考慮分片 |

### 1.3 明確的非目標

叢集排程 / bin-packing、多租戶資源配額、自動擴縮、通用工作負載編排。這些是 Nomad / Kubernetes 的領域；若未來需要，資料模型與其同構，屬遷移而非重寫。

---

## 2. 技術選型與理由

| 元件 | 選型 | 理由摘要 |
|---|---|---|
| Agent | Go 靜態二進位 | 無 runtime 依賴、bootstrap 即 `curl+chmod`；goroutine 適合監督+串流併發；基礎設施 agent 生態成熟（Nomad/kubelet 同語言） |
| 控制面 | Go（HTTP + gRPC） | 與 agent 共用 proto 與型別；標準庫 + `pgx`/`sqlc` 即足 |
| 對機器協議 | gRPC 雙向串流 | 高頻、需串流、對端是機器 |
| 對人協議 | HTTP/JSON（建議 Connect） | 低頻、瀏覽器直連、需可 curl、錯誤要可讀 |
| 前端 | Svelte + TypeScript | 編譯期框架、bundle 小、reactive 適合實時狀態面板 |
| 狀態庫 | PostgreSQL | 期望狀態（spec）+ 實際狀態（status）+ 審計日誌 |
| 指標庫 | Prometheus / VictoriaMetrics | 時序指標；UI 圖表可直接交給 Grafana |
| 制品庫 | S3 / MinIO | 內容定址、持久化；PG 存 metadata |

**為什麼不用現成方案**：直接用 Kubernetes/Nomad 需引入整套發行版與容器化，且交易特化的生命週期（撤單 hook、fencing lease、交易時段約束）在其上表達彆扭。自建成立的前提是「只做 kubelet 的 5%」，把省下的複雜度預算花在交易語義上。

---

## 3. 系統總覽

```
                       ┌──────────────────────────────────────┐
   Trader              │            控制面 Control Plane        │
  ┌────────┐  HTTP/JSON│  ┌──────────┐  ┌──────────┐          │
  │ Web UI │──────────▶│  │ 人向 API  │  │ 部署編排器 │          │
  │(Svelte)│           │  │ HTTP/JSON│  │  狀態機   │          │
  └────────┘           │  └──────────┘  └──────────┘          │
  ┌────────┐           │  ┌──────────┐  ┌──────────┐          │
  │  CLI   │──────────▶│  │ 排程分發器 │  │ gRPC 串流 │◀───┐    │
  └────────┘           │  └──────────┘  │  端點     │    │    │
                       │  ┌───────────────────────┐ └─────┼──┐ │
                       │  │ PostgreSQL(spec/status)│       │  │ │
                       │  │ TSDB · 制品庫 S3/MinIO  │       │  │ │
                       │  └───────────────────────┘       │  │ │
                       └──────────────────────────────────┼──┼─┘
                                          gRPC 雙向串流     │  │
                              (agent 出站發起，控制面無入站) │  │
                       ┌──────────────────────────────────┼──┼─┐
                       │        交易機器 × N               │  │ │
                       │  ┌─────────┐  監督   ┌──────────┐ │  │ │
                       │  │  Agent  │────────▶│ 策略進程 A │ │  │ │
                       │  │ 收斂迴圈 │────────▶│ 策略進程 B │ │  │ │
                       │  └────┬────┘         └──────────┘ │  │ │
                       │       │  本地版本快取 releases/vN   │  │ │
                       │       │  current → symlink        │  │ │
                       └───────┴───────────────────────────┴──┴─┘
```

三個元件、三條性質不同的連線。真相流向：**控制面的期望狀態是唯一真相，agent 持續把本地實際狀態收斂到期望狀態。**

---

## 4. 網路與連線拓樸

系統中有三條連線，各自按客戶端與頻率選協議——**協議跟著客戶端走**。

| 連線 | 協議 | 特性 | 暴露面 |
|---|---|---|---|
| Agent ↔ 控制面（南北向） | gRPC 雙向串流 | 高頻、機器、需串流 | agent 出站；控制面**無入站** |
| Trader ↔ 控制面（人向） | HTTP/JSON（Connect） | 低頻、瀏覽器、需可 curl | 置於 VPN/overlay 後 + SSO |
| Agent ↔ 本地策略（探活） | unix socket | 同機、極低摩擦 | 純本地 |

### 4.1 公網暴露原則

**agent 是出站的，控制面就不需要公網入站。** 這個不對稱是安全設計的核心——只有控制面需要被連到，且只被 agent 連到。

- **同 VPC（最常見）**：控制面只綁私有 IP，安全組僅放行 agent 網段，公網表面積為零。
- **跨區**：視雲廠商而定。GCP 的 VPC 是全域資源，不同 region 可同 VPC；AWS/阿里雲 VPC 綁 region，需 VPC Peering（2–3 區）或 Transit Gateway/CEN（多區且會成長）。
- **跨雲 / 有地端**：用 overlay（WireGuard/Tailscale）把所有機器與控制面拉進私有網段，控制面對公網隱身。一套方案吃所有拓樸。
- **降級方案**：無法用 overlay 時才選「公網端點 + 雙向 mTLS + 出口 IP 白名單 + 短 TTL enrollment token」，嚴格劣於前述。

因 agent 連線是低頻控制流，各私網路徑的延遲差異對它幾乎無感；選型純按管理複雜度與未來拓樸考量。策略進程到交易所的低延遲連線是**完全獨立**的另一件事，由 colocation 決定。

**鐵律**：控制面永遠不主動向 agent 發起入站連線；所有南向指令沿已建立的出站串流回推。

---

## 5. 機器註冊與身分

```
1. Trader 開機 → 執行 bootstrap（帶控制面簽發的一次性 enrollment token）
2. Agent 啟動 → 生成金鑰對 → 以 token 換取長期 mTLS 憑證
3. 之後所有通訊走 mTLS（機器身分可撤銷；token 洩漏僅短暫窗口）
4. Agent 建立 gRPC 雙向串流 → 送 Register → 進入心跳 + 收斂迴圈
```

---

## 6. 收斂模型（架構靈魂）

### 6.1 level-triggered，不是 edge-triggered

Agent **不是** RPC handler（收到「部署 v42」就執行動作），而是收斂迴圈：持續比較期望狀態與實際狀態，計算差異，執行收斂。

**這帶來的性質**：重連、重啟、丟包全部退化成同一個無聊情況——拿最新期望狀態快照，收斂一次，結束。災難復原的簡單性全靠這個。

**實作紀律**：每個南向訊息都路由成「更新本地期望狀態副本 → 觸發收斂」，而不是直接動手執行。寫 RPC handler 直覺上更順手，要刻意避免。

### 6.2 spec / status 分離

| | 誰能寫 | 內容 |
|---|---|---|
| spec（期望狀態） | 只有控制面 | 該機器應跑哪些策略、版本、配置、cron |
| status（實際狀態） | 只有 agent | 觀察到的實際版本、健康狀況、observedVersion |

控制面比對 `spec.version` 與 `status.observedVersion` 即知收斂進度——UI 上「部署卡在哪一步」的資訊全來自此比對。**絕不把期望與實際混在同一欄位讀寫**（經典收斂 bug 溫床）。

### 6.3 帶版本號的 watch + 定期 resync

- 每份 DesiredState 帶單調遞增版本號；agent 記住最後處理的版本。
- 串流斷線重連 → 控制面直接推當前全量快照（冪等，無需斷點續傳）。
- 定期全量 resync（每幾分鐘無條件推一次），對抗「串流沒斷但訊息悄悄丟」的靜默偏差（k8s informer 標準防禦）。

---

## 7. 通訊協議

自訂窄 gRPC 協議（不複用 CRI / k8s API，兩者形狀都不對）。借鑑 k8s 的**設計語彙**（metadata/spec/status 三段式、condition list、observedGeneration），但不 import 其依賴。

### 7.1 訊息集合

```
北向 (agent → 控制面):
  Register        機器上線，帶硬體規格
  Heartbeat       資源指標 + 各進程狀態（5–10s）
  StatusReport    收斂結果 / 部署狀態機轉移
  EventLog        結構化事件（審計）
  NACK            收到不認識的指令時顯式拒絕（不可靜默忽略）

南向 (控制面 → agent):
  DesiredState    完整期望狀態快照 + 版本號（主要機制）
  TriggerRollback 帶內即時指令（優化，非真相；可由改 DesiredState 等價表達）
  DrainNow        同上
```

### 7.2 設計要點

- **南向盡量只推 DesiredState 全量快照**，落實 level-triggered 收斂。指令式操作僅為即時性優化，且必須能被「改期望狀態版本指向」等價表達。
- **Proto 只做加法演進**：新欄位、新訊息，永不改號、永不改語義。
- **窄協議是自建的紅利**：欄位少、語義全懂、表面積可控。別把它讓給 CRI。

### 7.3 Connect 方案（對人 API）

用 [Connect](https://connectrpc.com/) 讓同一份 proto 同時服務 gRPC、gRPC-Web 與純 HTTP/JSON POST。一份 schema 三種吃法，curl 可直接打 JSON——避免「gRPC + 手寫 REST」兩套介面定義。

---

## 8. 部署、版本佈局與回滾

### 8.1 不可變版本 + 原子切換

CI 產出策略二進位（Go 單一靜態檔）連同 SHA256 上傳制品庫。機器上的目錄佈局：

```
/opt/strategies/<name>/
  releases/v41/  v42/  v43/     # 各含 binary + config snapshot
  current -> releases/v42        # symlink（原子切換點）
  shared/                        # 跨版本資料（狀態檔、日誌）
```

### 8.2 部署狀態機

```
pending → downloading → verifying(校驗 SHA)
        → draining(通知舊進程優雅退出、撤單/平倉 hook)
        → switching(原子改 symlink)
        → starting → health_checking
        → healthy | rolled_back
```

每個轉移寫回控制面，UI 上 trader 能看到卡在哪一步。

### 8.3 回滾是 O(1)

symlink 指回上一版、重啟進程，**無需重新下載**。

- **手動**：trader 一鍵。
- **自動**：新版本啟動後若在觀察窗（如 120s）內健康檢查連續失敗或崩潰超過 N 次，agent 自行切回上一版並標記該版本 `bad`，防止收斂迴圈反覆拉起壞版本。

### 8.4 配置即制品

配置也是版本化制品。改配置 = 發佈一個新 release（binary 不變、config 變），走**完全相同**的路徑——因此配置回滾免費獲得，且每次變更可審計。

---

## 9. 進程監督與健康檢查

### 9.1 監督核心 = supervisord 核心 + kubelet 收斂語義

Agent 自己 fork/exec 並監督策略進程（而非委託 systemd）。可抄襲 supervisord 的成熟設計：
- 崩潰指數退避重啟 + `startsecs`（進程要活過 N 秒才算啟動成功，避免快速崩潰迴圈被誤判為成功重啟）。
- `stopwaitsecs` 優雅關閉時序（SIGTERM → 等待 → SIGKILL，中間插撤單 hook）。
- stdout/stderr 接管與輪轉。

策略進程啟動時 `setsid` 放進獨立 session/process group（自我更新的前提，見 §11）。systemd 只管 agent 本身。

### 9.2 三層健康檢查

| 層 | 機制 | 抓什麼 |
|---|---|---|
| Liveness | agent `wait`/pidfd 子進程 | 進程是否活著（掛了立刻知道） |
| Readiness | 策略本地 health endpoint（unix socket/HTTP） | 已連行情、已連交易所、佇列未積壓 |
| 業務健康 | 策略主動 heartbeat 帶業務指標 | 最後行情時間戳、掛單數、當日 PnL |

**業務健康層最關鍵**：「進程活著但 30 秒沒收到行情」這種半死狀態只有這層抓得到，而它是交易系統最常見的故障模式。

### 9.3 告警分級

「機器離線」與「進程掛了」是不同等級。對交易系統，前者更危險——你失去觀測能力但策略可能還在下單。控制面若 3 個心跳週期沒收到訊號即標記 `unreachable` 並告警。

---

## 10. 定時任務

- Cron 定義存控制面（可審計、可統一修改），但**下發到 agent 本地執行**。
- **關鍵取捨**：若 00:00 重啟依賴控制面即時下指令，控制面故障時所有機器日常維護全部停擺。本地執行則控制面只負責分發 spec 與收集執行記錄。
- Spec 含標準 crontab 語法 + **顯式時區**（加密市場 00:00 UTC ≠ 台北 00:00）+ 動作類型（restart / reload config / 自訂腳本）。
- 多台跑同策略時加**隨機抖動或分批**，避免同時全下線。

---

## 11. Agent 自我更新

平台自身也要更新，且更新失敗時無人來救——雞生蛋問題。把 agent 自己當成受管制品，複用 §8 的目錄佈局，加接管協議。

### 11.1 接管流程

```
1. 下載 agent v(N+1) → SHA256 驗證 → 切 symlink
2. 舊 agent 持久化監督狀態檔（pid + starttime + 策略元數據）
3. 舊 agent 乾淨退出；策略進程因 setsid 獨立存活，不受影響
4. systemd 拉起新 agent → 讀狀態檔 → 用 pidfd 重新接管監控
5. 看門狗觀察窗：T 秒內連回控制面且健康？
     是 → 回報 v(N+1)，控制面推進下一批 canary
     否 → symlink 切回 vN，標記壞版本，凍結該批次
```

### 11.2 關鍵細節

- **進程解耦是前提**：策略 `setsid` 進獨立 process group，agent 退出時不連帶終止它們（必須第一天就做對）。
- **pidfd 重新接管**：新 agent 非舊進程父進程，用 Linux 5.3+ `pidfd_open` 對任意進程拿可 poll 的 fd；收屍由 systemd 代勞。狀態檔存 `/proc/<pid>/starttime`，接管前比對防 pid 重用。
- **看門狗在 agent 之外**：新 agent 若壞到起不來無法自行回滾，故回滾邏輯放 systemd `ExecStartPre` guard 腳本——檢查啟動計數，連續 M 次未在 T 秒內寫 healthy marker 就切回上一版（嵌入式 A/B 分區升級的用戶態版本）。
- **canary 批次**：agent 版本也是期望狀態的一部分。先推 1 台非關鍵機器 → 10% → 全量。永不全量同時更新（唯一能把「單機失敗」放大成「全平台失聯」的途徑）。

### 11.3 更新順序

**永遠先更控制面、後更 agent**。控制面向後相容舊 agent 容易，反過來讓舊 agent 理解新控制面不可能。

---

## 12. 版本偏差管理

自我更新意味著系統永遠混版本：控制面 vX 同時服務 agent vN、vN-1、vN-2。

- Proto 只做加法演進（見 §7.2）。
- Agent 心跳回報自身版本；控制面對舊 agent 做**顯式降級**（功能標記 `min_agent_version`，不滿足則拒絕下發並在 UI 標示「agent 過舊」）。
- Agent 收到不認識的指令回 **NACK**，不可靜默忽略（否則控制面誤以為部署成功）。
- 監督狀態檔（§11）自身也有 schema 版本相容問題：舊 agent 寫的狀態檔要能被新 agent 讀懂。

---

## 13. 災難復原

### 13.1 控制面 DR

- 控制面設計成**無狀態**（真相全在 PG）。要 HA 就跑兩實例 + LB。
- PG 做 WAL 歸檔 + 每日 base backup；制品庫本身是 S3 級持久化。
- 重建後 agent 重連即恢復（level-triggered 的紅利）。

### 13.2 機器 DR

- 機器重開機 → systemd 拉起 agent → 拉取自己的期望狀態 → 本地版本快取還在就直接恢復進程。
- 整機蒸發 → trader 開新機器 → bootstrap → 在 UI 把死機器策略指派過去；版本與配置都在制品庫，新機器幾分鐘收斂到位。

### 13.3 防雙開（交易特有的坑）

舊機器可能只是網路分區而非真死，策略在兩台機器同時下單是災難。

**解法：fencing lease。** 策略啟動前向控制面取一個帶 TTL 的 lease（fencing token），續不上就自殺；控制面確認舊 lease 過期才允許新機器啟動同一策略。

---

## 14. 執行方式：Execution Driver 抽象

平台不綁定執行方式，做一層薄的 driver 抽象。

- **預設 driver = 裸進程 + cgroup v2**：Go 靜態二進位無依賴，Docker 的依賴打包對它零價值；cgroup v2 直接給資源隔離；監督鏈最短（agent → 進程一跳直達 pidfd）。
- **可選 driver = OCI image**（containerd client / podman，daemonless；host networking）：為 Python/ML 策略而留——帶 PyTorch/CUDA 的研究型策略，依賴打包從零價值變核心價值。
- **不用 dockerd**：額外常駐 daemon 會給監督模型加一個你不控制的中間層，破壞「agent 更新不影響策略」的性質。

**落地優先序**：v1 只實作 exec driver，但 proto 與狀態庫的 artifact 定義從第一天就留 `type`（`binary` | `oci_image`）與 `driver` 欄位。加 driver 是純增量工作，不需動資料模型。判斷是否需提前做容器 driver 的訊號：策略組合裡有沒有非 Go 的東西排隊上線。

---

## 15. 前端架構（Svelte）

### 15.1 職責

實時艦隊狀態面板、策略發佈操作、部署進度追蹤、回滾觸發、cron 管理、審計日誌檢視。

### 15.2 技術選型

| 面向 | 選擇 | 理由 |
|---|---|---|
| 框架 | Svelte + SvelteKit | 編譯期 reactive、bundle 小、適合實時狀態綁定 |
| API 客戶端 | Connect-ES（TypeScript） | 從同一份 proto 生成型別安全客戶端，與後端 schema 單一真相 |
| 實時更新 | Server-Sent Events / gRPC-Web streaming | 機器狀態、部署進度推送 |
| 圖表 | 交給 Grafana（嵌入或連結） | 指標圖表直接讓 Grafana 讀 TSDB，省掉自繪實時圖表工程量 |

### 15.3 狀態管理

- 期望狀態與實際狀態在前端也分離呈現（呼應 §6.2）：UI 明確區分「你要求的」與「機器實際跑的」，兩者不一致時高亮顯示收斂中/滯後。
- 部署狀態機（§8.2）在 UI 上呈現為進度步驟，每步對應後端回報的轉移。

### 15.4 頁面結構（建議）

```
/                     艦隊總覽（機器列表 + 資源實時指標）
/machines/:id         單機詳情（受管進程、資源歷史、事件日誌）
/strategies           策略列表與版本
/strategies/:id/deploy 發佈流程（選版本/配置 → 觀察狀態機）
/strategies/:id/rollback 回滾（選目標版本）
/schedules            cron 管理
/audit                審計日誌
```

---

## 16. 後端架構（Go）

### 16.1 模組劃分

```
cmd/
  controlplane/       控制面 main
  agent/              agent main
internal/
  proto/              共用 protobuf 生成碼（agent + 控制面 + 前端源頭）
  controlplane/
    api/              人向 HTTP/JSON（Connect handler）
    grpcstream/       南北向 gRPC 雙向串流端點
    orchestrator/     部署狀態機
    scheduler/        cron spec 分發
    store/            PG 存取（pgx/sqlc）：spec / status / audit
    artifacts/        制品庫 metadata + S3/MinIO 客戶端
    metrics/          TSDB 寫入
  agent/
    reconciler/       收斂迴圈（單一 goroutine 狀態機）
    driver/           execution driver 介面 + exec 實作（cgroup/setsid/pidfd）
    supervisor/       進程監督（退避重啟、優雅關閉、日誌接管）
    health/           三層健康檢查
    cron/             本地 cron 執行器
    selfupdate/       自我更新接管協議
    stream/           gRPC 串流客戶端（重連、resync）
```

### 16.2 收斂迴圈實作（agent 核心）

單一 goroutine 持有本地期望狀態副本，序列化處理三類事件：收到新 DesiredState、進程退出通知（pidfd 可讀）、定時 tick（cron / resync / 健康檢查）。序列化避免併發修改實際狀態的競態。每次事件後計算 diff → 執行收斂 → 回報 status。

### 16.3 資料模型（PostgreSQL，概念層）

```
machines        機器身分、憑證指紋、最後心跳、可達性
strategies      策略定義、當前指派的機器
artifacts       版本化制品（type: binary|oci_image、SHA256/digest、S3 key）
desired_state   spec：機器 × 策略 × 版本 × 配置 × cron（含版本號）
observed_state  status：agent 回報的實際版本、健康、observedVersion
schedules       cron spec（crontab + 時區 + 動作）
leases          fencing lease（策略 × 機器 × TTL）
audit_log       每個部署/回滾：誰、何時、什麼版本（第一天就要有）
```

---

## 17. 開發優先序（建議里程碑）

1. **協議先行**：定 proto——`AgentService` 雙向串流訊息（Register/Heartbeat/DesiredState/StatusReport）是整個系統的憲法。
2. **收斂骨架**：agent 收斂迴圈 + 控制面串流端點 + PG spec/status，跑通「改期望狀態 → agent 收斂」的空迴圈。
3. **exec driver + 監督**：setsid/cgroup/pidfd + 退避重啟 + 三層健康檢查。
4. **部署與回滾**：制品庫 + 版本佈局 + 部署狀態機 + O(1) 回滾。
5. **人向 API + Svelte UI**：Connect handler + 艦隊面板 + 發佈流程。
6. **cron 本地執行 + fencing lease**。
7. **自我更新接管協議 + canary**。
8. **DR 演練**：控制面重建、機器重開、整機重新指派、雙開防護驗證。

---

## 18. 規模成長時的重看點

- gRPC 串流數在數百台內單控制面無問題；上千台再考慮分片。
- 審計日誌一開始就做，事後補非常痛。
- 策略間資源隔離從「約定」升級到 cgroup v2 強制限制的時機。
- 當你發現自己在 agent 裡重新發明 scheduler 或 service discovery 時，就是該遷移到 k3s/Nomad 的訊號——因資料模型同構，遷移是翻譯而非重寫。

---

## 附錄 A：關鍵設計決策速查

| 決策 | 選擇 | 一句話理由 |
|---|---|---|
| 真相方向 | 遠端期望狀態為真相 | 發佈/回滾/DR 統一為收斂 |
| 觸發模型 | level-triggered | 重連/重啟/丟包退化成同一情況 |
| 連線發起方 | agent 出站 | 控制面無需公網入站 |
| 對機器協議 | gRPC 串流 | 高頻、機器、串流 |
| 對人協議 | HTTP/JSON（Connect） | 低頻、瀏覽器、可 curl |
| 版本切換 | 不可變 release + symlink | 回滾 O(1) |
| 配置 | 版本化制品 | 配置回滾免費 |
| 進程監督 | agent 自監督（非 systemd） | 細粒度退避/撤單 hook/pidfd |
| 執行方式 | driver 抽象，預設裸進程 | 依賴打包對 Go 二進位零價值 |
| 防雙開 | fencing lease | 網路分區時防同策略雙下單 |
| 自我更新 | pidfd 接管 + systemd guard 回滾 | 更新失敗自動退回而非失聯 |
