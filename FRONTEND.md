# 策略發佈平台 — 人向 API 與前端介面

> 版本 v0.1 · 搭配 ARCHITECTURE.md §15、PROTOCOL.md §8、與 repo 的 `control_service.proto` 閱讀
>
> 本文件定義兩件事:(1) 控制面對「人」(Svelte UI / CLI)暴露的 HTTP API,基於 Connect,含 server-streaming;(2) 前端介面的頁面、狀態模型與即時更新機制。這是 foundation 之上的第一層,不碰 lease / 進程監督等安全關鍵部分,純粹把已有的收斂狀態渲染與觸發。

---

## 0. 立場與邊界

- **這是與 `AgentService` 不同性質的連線**:低頻、瀏覽器/CLI、需可 curl。走 HTTP/JSON via Connect,與 agent 的 gRPC 串流分開端口與心智。
- **一份 proto 三方共用**:前端用 Connect-ES 從 `control_service.proto` 生成 TypeScript 客戶端,型別與後端單一真相。改欄位 → proto 一改 → 前後端重新生成 → 編譯期抓不一致。
- **即時更新走 Connect server-streaming,不走 WebSocket**:面板流量嚴重不對稱(server 持續推狀態、client 零星送指令),用不到 WebSocket 的全雙工;server-streaming 推狀態 + unary 送指令是更一致的組合。
- **現階段誠實範圍**:底層是 in-memory store(重啟即失),因此本層是「即時運維面板」而非「歷史/安全面板」。lease 狀態欄位存在但 agent 尚未填(SAFETY 未實作),UI 顯示為 unknown。認證留待 §7,開發期綁 `127.0.0.1`。

---

## 1. proto 現況與必要增補

`control_service.proto` 已定義 `ControlPlaneService`(ListMachines / GetMachine / Deploy / Rollback / SetSchedule / WatchMachine / ListAudit)。有一個**缺口必須先補**,否則前端拿不到最想要的資料。

### 1.1 缺口:MachineStatusEvent 不含策略狀態

現況 `MachineStatusEvent` 只帶 `Machine`(機器層 metadata + 資源),但**前端的核心畫面是部署進度**,而那是 per-strategy 的 `StrategyAssignmentStatus`(`phase`、`running_artifact`、`conditions`、`observed_generation`)——機器層看不到。`Machine` 訊息裡也沒有策略狀態列表。

### 1.2 增補(加法演進,遵守 PROTOCOL §9)

```proto
// 在 Machine 加上策略狀態列表(新欄位,不動既有欄位號)
message Machine {
  ObjectMeta metadata = 1;
  MachineSpec spec = 2;
  MachineResources last_resources = 3;
  bool reachable = 4;
  int32 agent_version = 5;
  google.protobuf.Timestamp last_heartbeat = 6;
  repeated StrategyView strategies = 7;   // 新增:每策略的期望+實際合併視圖
}

// 前端友善的合併視圖:把 spec 的目標與 status 的實際並排,直接對應 UI 的
// 「你要求的 vs 機器實際跑的」。控制面組裝,前端不必自己 join。
message StrategyView {
  string strategy = 1;
  // 期望(來自 spec)
  ArtifactRef desired_artifact = 2;
  ArtifactRef desired_config = 3;
  int64 spec_generation = 4;
  // 實際(來自 agent 回報的 status)
  DeployPhase phase = 5;
  ArtifactRef running_artifact = 6;
  ArtifactRef running_config = 7;
  int64 observed_generation = 8;
  repeated Condition conditions = 9;
  int32 pid = 10;
  int32 restart_count = 11;
  string last_error = 12;
  // 收斂判定(控制面算好,前端直接用)
  bool converged = 13;                    // desired == running && phase==HEALTHY
  bool lease_held = 14;                   // agent 未填時 UI 顯示 unknown
  google.protobuf.Timestamp lease_expires_at = 15;
}
```

> 設計重點:**控制面替前端做好 spec/status 的 join,吐一個合併的 `StrategyView`。** 前端不該自己拿 spec 一份、status 一份再對齊——那是把收斂判定邏輯洩漏到前端。`converged` 由控制面用與 agent 相同的 digest 比對算出(呼應 RECONCILER 的 `versionMatches`),前端只負責渲染。

---

## 2. HTTP API 總覽

Connect 讓同一 service 同時接受三種呼叫方式,前端與 curl 各取所需:

| 呼叫端 | 方式 | 用途 |
|---|---|---|
| Svelte UI | Connect-ES 生成客戶端(gRPC-Web / Connect protocol) | unary 指令 + server-streaming 訂閱 |
| CLI / curl | 純 HTTP/JSON POST | 腳本化、debug |

**端點形狀**(Connect 慣例):`POST /strategyplatform.v1.ControlPlaneService/{Method}`,body 為 JSON,回應為 JSON。

### 2.1 讀路徑(unary)

**ListMachines** — 艦隊列表
```bash
curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/ListMachines \
  -H "Content-Type: application/json" -d '{"pageSize":50}'
```
回:`{ "machines": [ { "metadata":{...}, "reachable":true, "strategies":[...] } ], "nextPageToken":"" }`

**GetMachine** — 單機詳情(含策略合併視圖)
```bash
curl -sX POST .../ControlPlaneService/GetMachine -d '{"machineId":"m1"}'
```

**ListAudit** — 審計(in-memory 期間可能為空)
```bash
curl -sX POST .../ControlPlaneService/ListAudit -d '{"machineId":"m1","pageSize":100}'
```

### 2.2 寫路徑(unary,回 generation 供追蹤)

寫操作一律經控制面:驗證 → 更新 spec → `generation++` → 沿 agent 串流推新 DesiredState。回傳的 `generation` 是前端追蹤收斂的錨——前端拿它比對後續串流事件裡的 `observed_generation`,判斷「我這次操作收斂了沒」。

**Deploy**
```bash
curl -sX POST .../ControlPlaneService/Deploy \
  -d '{"machineId":"m1","strategy":"s","artifactVersion":"v42","configVersion":""}'
# → {"generation":"1235"}
```

**Rollback**（`targetVersion` 空 = 上一版）
```bash
curl -sX POST .../ControlPlaneService/Rollback \
  -d '{"machineId":"m1","strategy":"s","targetVersion":""}'
```

**SetSchedule**（全量覆蓋該策略 cron;cron 執行器未實作前僅寫 spec）
```bash
curl -sX POST .../ControlPlaneService/SetSchedule \
  -d '{"machineId":"m1","strategy":"s","schedules":[{"name":"daily-restart","cronExpr":"0 0 * * *","timezone":"UTC","action":"CRON_ACTION_RESTART","jitterSeconds":30}]}'
```

### 2.3 即時路徑(server-streaming)

**WatchMachine** — 訂閱單機的狀態推送。連線建立時先推一次當前全量快照(對齊 agent 串流的 resync 心智),之後狀態每變一次推一個事件。

```bash
# curl 也能看(Connect streaming 以 JSON 逐幀回)
curl -N -sX POST .../ControlPlaneService/WatchMachine -d '{"machineId":"m1"}'
```

每個 `MachineStatusEvent` 帶完整的 `Machine`(含 §1.2 的 `strategies`),前端據此重繪。**推送觸發點**:agent 的 StatusReport / Heartbeat 更新了 store、或人向寫操作改了 spec generation。

> 為何推「全量 Machine」而非 diff:與 agent 串流同一個理由——全量快照冪等,斷線重連直接重推當前狀態,前端不需要精巧的增量合併與斷點續傳。機器層資料量小,全量成本可忽略。

---

## 3. 前端狀態模型

### 3.1 核心原則:期望與實際分離呈現

呼應 RECONCILER §0 / SAFETY 的 spec/status 分離,**前端也把「你要求的」與「機器實際跑的」分開顯示,不一致時高亮**。這不是美學選擇,是讓 trader 一眼看出「收斂中/滯後/卡住」的關鍵。

```
每個策略卡片:
  期望:  v42  (spec_generation 1235)
  實際:  v41  (running, observed_generation 1234)  ← 高亮:落後一代
  階段:  SWITCHING  ← 收斂進行中
```

`converged=true` 時兩行一致、階段 HEALTHY、無高亮。`converged=false` 時前端明確標示不一致與當前 phase,不隱藏這個中間態。

### 3.2 連線與重連

- 每個開著的 Machine 詳情頁 = 一條 `WatchMachine` 串流。
- 串流斷線 → 指數退避重連 → 重連時後端先推全量快照 → 前端整頁狀態以快照為準覆蓋(last-write-wins,current wins over stale)。
- 前端**不維護跨串流的增量狀態**——每個事件的 `Machine` 就是當前完整真相,直接替換。這消除了前端側的狀態合併 bug。

### 3.3 操作的樂觀反饋 + generation 追蹤

trader 按「Deploy v42」:
1. 前端送 `Deploy` unary,拿回 `generation=1235`。
2. UI 立即把該策略標為「部署中(目標 v42, gen 1235)」——樂觀反饋,不等收斂。
3. `WatchMachine` 串流開始推 phase 轉移(PENDING→…→HEALTHY),前端據 `observed_generation` 追上 1235 且 phase=HEALTHY 時,標為「已收斂」。
4. 若串流推來 `phase=FAILED` 或 `ROLLED_BACK`,UI 顯示失敗/已回滾與 `last_error`。

**關鍵**:操作的成功不由 unary 回應判定(那只代表「控制面收到並改了 spec」),而由**後續串流事件裡 observed_generation 追上 + phase 到 HEALTHY** 判定。這正確反映了 level-triggered 的本質——下指令 ≠ 完成,收斂才算完成。

---

## 4. 頁面結構

先做即時、後做歷史(歷史依賴 Postgres,見 §6)。

```
/                     艦隊總覽 —— ListMachines 輪詢或多機串流;機器卡片+可達性+資源
/machines/:id         單機詳情 —— WatchMachine 串流;策略卡片(期望vs實際)、資源、事件
/machines/:id/:strat  策略部署追蹤 —— DeployPhase 狀態機視覺化(本層最該先做)
/deploy               發佈流程 —— 選機器/策略/版本 → Deploy → 觀察 phase 推進
/schedules            cron 管理 —— SetSchedule(執行器未實作前僅寫 spec,UI 標示 pending)
/audit                審計 —— ListAudit(Postgres 接上前為空,UI 標示)
```

### 4.1 優先做:策略部署追蹤(`/machines/:id/:strat`)

這是整個系統最值得先做的畫面,因為它把 level-triggered 設計最直觀地展示出來:**部署時 DeployPhase 一格格推進的實時視覺化**。

```
[PENDING]→[DOWNLOADING]→[VERIFYING]→[DRAINING]→[SWITCHING]
   →[STARTING]→[HEALTH_CHECKING]→[HEALTHY]
                                        ↓(失敗)
                              [ROLLING_BACK]→[ROLLED_BACK]
```

每格對應 `StrategyView.phase`,當前格高亮,已過的格打勾,失敗分支變紅。資料全來自 `WatchMachine` 串流,無需輪詢。這個畫面做通,就證明了「後端串流 → 前端渲染」整條鏈路成立,也是驗證 §1.2 proto 增補是否到位的試金石。

### 4.2 艦隊總覽的即時性取捨

總覽頁 N 台機器,兩種做法:(a) 每台一條 `WatchMachine` 串流——即時但連線數多;(b) `ListMachines` 定時輪詢(如 2s)——簡單但有延遲。建議總覽用輪詢(總覽不需要秒級精度)、詳情頁用串流(進單機才需要 phase 級即時)。未來機器多時可加一個 `WatchFleet` 串流方法合併推送,但現在不必。

---

## 5. 技術選型(前端)

| 面向 | 選擇 | 理由 |
|---|---|---|
| 框架 | Svelte + SvelteKit | 編譯期 reactive、bundle 小、狀態綁定直觀 |
| API 客戶端 | Connect-ES(`@connectrpc/connect-web`) | 從 `control_service.proto` 生成型別安全客戶端;unary + server-streaming 同一套 |
| 串流訂閱 | Connect server-streaming(`for await` async iterator) | 與後端協議一致,斷線重連模式同構於 agent 串流 |
| 圖表 | 資源指標可嵌 Grafana / 或 lightweight JS 圖 | 省自繪即時圖工程量;深度分析交給 Grafana 讀 TSDB |
| 狀態容器 | Svelte store per open machine | 每頁一條串流一個 store,事件替換式更新 |

### 5.1 Connect-ES 訂閱骨架

```ts
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { ControlPlaneService } from "./gen/strategyplatform/v1/control_service_connect";

const client = createClient(ControlPlaneService,
  createConnectTransport({ baseUrl: "http://127.0.0.1:8081" }));

// server-streaming:async iterator,斷線後外層重連
async function watch(machineId: string, onEvent: (m: Machine) => void, signal: AbortSignal) {
  for await (const ev of client.watchMachine({ machineId }, { signal })) {
    onEvent(ev.machine!);   // 每個事件的 machine 是完整真相,直接替換 store
  }
}
```

重連策略:`watch` 外層包指數退避迴圈,`AbortSignal` 控制頁面卸載時中止;重連後第一個事件是全量快照,store 直接覆蓋。

---

## 6. 與 database 的關係(為何前端先於 Postgres)

- 前端消費的是 `ControlPlaneService` API,**不是 store**。store 是 in-memory 還是 Postgres,前端一無所知——中間隔著 API 這道牆,兩者開發順序可任意。
- 現在對 in-memory store 做的前端,Postgres 接上後**一行不改**。
- 唯一例外是**依賴歷史的頁面**(`/audit`、歷史趨勢)——in-memory 下為空,UI 明確標示「歷史需持久化,尚未啟用」,並排在最後做。
- 做前端會**反過來產出真實的查詢需求清單**(要哪些欄位、怎麼篩、怎麼分頁),那份清單才是設計 Postgres schema 的正確輸入。先做前端 → 需求驅動 schema;先做 DB → 猜測驅動 schema 然後返工。

---

## 7. 認證(現階段留白但劃界)

- **現階段**:綁 `127.0.0.1`,無認證,本地開發用。**切勿**把這個 `/admin` 風格端口暴露公網。
- **上線前**:人向 API 置於 VPN/overlay 後 + SSO(ARCHITECTURE §15/§4.1),並加最基本的 authz——哪個 trader 能部署到哪台機器、能否看到別人的策略。這與 agent 的 mTLS 是兩套獨立機制(人 vs 機器),不可混用。
- Connect 支援 interceptor,認證與 authz 以 interceptor 掛在 service 前,unary 與 streaming 共用,不必為兩者分別實作。

---

## 8. 落地順序(每步可獨立驗證)

1. **補 proto**:`Machine.strategies` + `StrategyView`(§1.2),`make generate` 重生成 Go + TS。
2. **後端讀路徑**:`ListMachines` / `GetMachine` 對現有 in-memory store 實作,控制面組裝 `StrategyView`(做 spec/status join + `converged` 判定)。
3. **後端串流**:`WatchMachine` —— 連線推全量快照,store 變更時推事件。用一個 per-machine 的 fan-out(store 更新 → 通知所有訂閱者)。
4. **後端寫路徑**:`Deploy` / `Rollback` 正名(取代 `/admin/assign` 樁),寫 spec + bump generation + 推 DesiredState + 寫 audit。
5. **前端**:Connect-ES 生成客戶端 → 先做 `/machines/:id/:strat` 部署追蹤頁(驗證串流鏈路)→ 再做單機詳情與艦隊總覽 → 發佈流程。
6. **收尾**:cron 管理與 audit 頁做 UI 但標示後端未實作部分(誠實佔位)。

---

## 附錄:方法速查

| 方法 | 類型 | 用途 | 回傳 |
|---|---|---|---|
| ListMachines | unary | 艦隊列表 | machines[] + next_page_token |
| GetMachine | unary | 單機詳情(含 StrategyView) | Machine |
| Deploy | unary(寫) | 發佈版本 | generation |
| Rollback | unary(寫) | 回滾 | generation |
| SetSchedule | unary(寫) | 覆蓋 cron | generation |
| WatchMachine | **server-stream** | 即時狀態/部署進度 | stream MachineStatusEvent |
| ListAudit | unary | 審計(需 Postgres) | entries[] + next_page_token |
