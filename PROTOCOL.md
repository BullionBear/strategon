# 策略發佈平台 — 通訊協議規格

> 版本 v0.1 · 搭配 ARCHITECTURE.md 閱讀
>
> 本文件定義平台的線上格式（wire format）。proto 是 agent、控制面、前端三方的**單一真相**；三者的型別皆由此生成。

---

## 0. 設計原則（讀 proto 前先讀這節）

1. **spec / status 徹底分離**。`spec` 只有控制面能寫，`status` 只有 agent 能寫。兩者靠 `generation` / `observed_generation` 關聯。任何把「期望」與「實際」塞進同一欄位讀寫的做法都禁止。
2. **南向以 DesiredState 全量快照為主**。控制面不說「去部署 v42」，而是說「你的期望狀態是 {...}，generation=1234」。agent 自行 diff 並收斂。指令式訊息（TriggerRollback/DrainNow）僅為即時性優化，語義上都能被「改 spec 版本指向」等價表達。
3. **只做加法演進**。永不改欄位號、永不改欄位語義、永不刪欄位（改為 `reserved`）。詳見 §9。
4. **未知即顯式拒絕**。agent 收到不認識的指令回 `Nack`，絕不靜默忽略——靜默忽略會讓控制面誤判部署成功。
5. **借 k8s 的設計語彙，不借它的依賴**。`ObjectMeta` / `generation` / `observedGeneration` / `Condition` 的命名與語義沿用 k8s 慣例，但這是自訂的窄 proto，不 import CRI 或 k8s API。

---

## 1. Package 與通用約定

```proto
syntax = "proto3";

package strategyplatform.v1;

option go_package = "github.com/org/platform/gen/strategyplatform/v1;strategyplatformv1";

import "google/protobuf/timestamp.proto";
```

約定：

- 所有時間戳用 `google.protobuf.Timestamp`，UTC。
- 所有 enum 的 0 值一律為 `*_UNSPECIFIED`（proto3 要求，且用於偵測「對端沒設這個欄位」）。
- `generation` 為 int64 單調遞增，由控制面在**每次 spec 變更**時 +1。
- ID 類欄位（`machine_id`、`command_id`、`message_id`）為字串 UUID。

---

## 2. 通用型別

### 2.1 ObjectMeta / Condition

```proto
message ObjectMeta {
  string name = 1;
  string uid = 2;
  int64  generation = 3;                          // 控制面在 spec 變更時遞增
  map<string, string> labels = 4;
  google.protobuf.Timestamp created_at = 5;
}

// k8s 風格的健康表達；用 condition list 而非單一 bool
message Condition {
  string type = 1;                                // "Live" | "Ready" | "BusinessHealthy"
  ConditionStatus status = 2;
  string reason = 3;                              // 機器可讀短碼，如 "MarketDataStale"
  string message = 4;                             // 人可讀說明
  google.protobuf.Timestamp last_transition = 5;
}

enum ConditionStatus {
  CONDITION_STATUS_UNSPECIFIED = 0;
  CONDITION_STATUS_TRUE = 1;
  CONDITION_STATUS_FALSE = 2;
  CONDITION_STATUS_UNKNOWN = 3;
}
```

### 2.2 制品與執行方式

```proto
enum ArtifactType {
  ARTIFACT_TYPE_UNSPECIFIED = 0;
  ARTIFACT_TYPE_BINARY = 1;                       // Go 靜態二進位 tarball
  ARTIFACT_TYPE_OCI_IMAGE = 2;                    // 為 Python/ML 策略預留
}

message ArtifactRef {
  ArtifactType type = 1;
  string name = 2;                                // 制品名
  string version = 3;                             // 人可讀版本，如 "v42"
  string digest = 4;                              // "sha256:..."；內容定址，驗證免費
  string uri = 5;                                 // "s3://bucket/key" 或 registry ref
}

enum ExecutionDriver {
  EXECUTION_DRIVER_UNSPECIFIED = 0;
  EXECUTION_DRIVER_EXEC = 1;                      // 裸進程 + cgroup v2（預設）
  EXECUTION_DRIVER_OCI = 2;                       // containerd/podman，host network
}
```

> 設計注記：`config` 也是一個 `ArtifactRef`——配置即版本化制品，改配置就是換一個 config 制品的 digest，走與 binary 完全相同的部署路徑，因此配置回滾免費（見 ARCHITECTURE §8.4）。

### 2.3 資源限制與部署策略

```proto
message ResourceLimits {
  int64 cpu_millicores = 1;                       // 寫入 cgroup v2 cpu.max
  int64 memory_bytes = 2;                         // memory.max
  int32 max_open_files = 3;
}

message DeployPolicy {
  int32 startsecs = 1;                            // 進程須存活 N 秒才算「啟動成功」
  int32 health_window_seconds = 2;                // 自動回滾觀察窗
  int32 max_crashes_in_window = 3;                // 窗內崩潰超過此值觸發自動回滾
  int32 stop_grace_seconds = 4;                   // SIGTERM → 等待 → SIGKILL
  bool  enable_auto_rollback = 5;
}
```

### 2.4 Cron

```proto
message CronSchedule {
  string name = 1;
  string cron_expr = 2;                           // 標準 crontab 語法
  string timezone = 3;                            // IANA tz，必填且顯式（00:00 UTC ≠ 台北 00:00）
  CronAction action = 4;
  int32  jitter_seconds = 5;                      // 隨機抖動，避免多機同時下線
  string script_ref = 6;                          // 當 action=RUN_SCRIPT 時的制品引用
}

enum CronAction {
  CRON_ACTION_UNSPECIFIED = 0;
  CRON_ACTION_RESTART = 1;                        // 每日 00:00 重啟進程
  CRON_ACTION_RELOAD_CONFIG = 2;
  CRON_ACTION_RUN_SCRIPT = 3;
}
```

### 2.5 Lease（防雙開）

```proto
message LeaseSpec {
  bool  required = 1;                             // 該策略啟動前是否須持有 lease
  int32 ttl_seconds = 2;                          // lease TTL；agent 須在到期前續約，否則自殺
}
```

---

## 3. Spec：期望狀態（控制面 → agent）

### 3.1 單一策略指派

```proto
message StrategyAssignmentSpec {
  string strategy = 1;                            // 邏輯策略 id
  ArtifactRef artifact = 2;                       // 期望的 binary/image 版本
  ArtifactRef config = 3;                         // 期望的配置版本
  ExecutionDriver driver = 4;
  ResourceLimits limits = 5;
  DeployPolicy deploy_policy = 6;
  LeaseSpec lease = 7;
  repeated CronSchedule schedules = 8;
  repeated string args = 9;
  map<string, string> env = 10;
}
```

### 3.2 機器層期望狀態快照

```proto
message DesiredState {
  int64 generation = 1;                           // 單調遞增快照版本
  repeated StrategyAssignmentSpec assignments = 2;// 該機器應跑的全部策略（全量，非增量）
  int32 desired_agent_version = 3;                // agent 自我更新的目標版本（canary 用）
  google.protobuf.Timestamp issued_at = 4;
}
```

> 這是南向的主訊息。`assignments` 是**全量**列表——agent 收到後 diff 本地實際狀態：spec 有而本地沒有的要部署，本地有而 spec 沒有的要下線。這個全量語義讓重連/重啟/丟包全部退化為「拿最新快照收斂一次」，冪等且無序列依賴。

---

## 4. Status：實際狀態（agent → 控制面）

### 4.1 部署狀態機

```proto
enum DeployPhase {
  DEPLOY_PHASE_UNSPECIFIED = 0;
  DEPLOY_PHASE_PENDING = 1;
  DEPLOY_PHASE_DOWNLOADING = 2;
  DEPLOY_PHASE_VERIFYING = 3;                     // 校驗 SHA256 / digest
  DEPLOY_PHASE_DRAINING = 4;                      // 通知舊進程優雅退出、撤單/平倉 hook
  DEPLOY_PHASE_SWITCHING = 5;                     // 原子改 symlink
  DEPLOY_PHASE_STARTING = 6;
  DEPLOY_PHASE_HEALTH_CHECKING = 7;
  DEPLOY_PHASE_HEALTHY = 8;
  DEPLOY_PHASE_ROLLED_BACK = 9;
  DEPLOY_PHASE_FAILED = 10;
}
```

### 4.2 單一策略的實際狀態

```proto
message StrategyAssignmentStatus {
  string strategy = 1;
  DeployPhase phase = 2;
  int64 observed_generation = 3;                  // 此 status 反映的 spec generation
  ArtifactRef running_artifact = 4;               // 實際正在跑的版本（可能滯後於 spec）
  ArtifactRef running_config = 5;
  repeated Condition conditions = 6;              // Live / Ready / BusinessHealthy
  int32 pid = 7;
  int32 restart_count = 8;
  google.protobuf.Timestamp started_at = 9;
  string last_error = 10;
  bool  lease_held = 11;
  google.protobuf.Timestamp lease_expires_at = 12;
}
```

> 控制面比對 `spec.generation`（來自 DesiredState）與 `status.observed_generation` 即知該策略收斂到哪、是否滯後——UI 上「部署卡在哪一步」的資訊全來自 `phase` + 這組比對。`running_artifact` 與 spec 的 `artifact` 不一致時，即代表收斂進行中或失敗。

---

## 5. 心跳與指標（agent → 控制面）

心跳高頻（5–10s），內容輕；`StatusReport` 事件驅動（狀態轉移時 + resync 時），內容重。兩者分開避免高頻訊息夾帶大 payload。

```proto
message MachineResources {
  double cpu_percent = 1;
  int64  memory_used_bytes = 2;
  int64  memory_total_bytes = 3;
  int64  disk_used_bytes = 4;
  int64  disk_total_bytes = 5;
  double load1 = 6;
  int64  net_rx_bytes = 7;
  int64  net_tx_bytes = 8;
  google.protobuf.Timestamp collected_at = 9;
}

message ProcessMetrics {
  string strategy = 1;
  int32  pid = 2;
  bool   alive = 3;
  int64  rss_bytes = 4;
  int32  num_fds = 5;
  double cpu_percent = 6;
  int32  restart_count = 7;
}

message Heartbeat {
  MachineResources resources = 1;
  repeated ProcessMetrics processes = 2;
  int64  observed_generation = 3;                 // agent 已完全收斂到的快照版本
  int32  agent_version = 4;                       // 每拍回報版本，供偏差追蹤
}
```

> `Heartbeat.processes` 用於 TSDB 與 liveness；`observed_generation` 讓控制面在不等 StatusReport 的情況下也能粗略追蹤收斂進度。控制面連續 3 個心跳週期沒收到即標記機器 `unreachable`——注意「機器離線」與「進程掛了」是不同告警等級。

---

## 6. 註冊與 enrollment

Enrollment 是一次性的憑證交換，走 bootstrap 通道（token 驗證）；成功後所有後續通訊走 mTLS。

```proto
message MachineSpec {
  int32  num_cpus = 1;
  int64  memory_total_bytes = 2;
  string os = 3;
  string arch = 4;
  string kernel_version = 5;
  string region = 6;
  string zone = 7;
  repeated ExecutionDriver supported_drivers = 8; // agent 宣告本機支援哪些 driver
}

message EnrollRequest {
  string enrollment_token = 1;                    // 控制面簽發的一次性短 TTL token
  string requested_machine_id = 2;
  bytes  csr = 3;                                 // 憑證簽章請求（PEM）；agent 本地生成金鑰對
  MachineSpec spec = 4;
}

message EnrollResponse {
  bytes  certificate = 1;                         // 簽發的長期 client cert（PEM）
  bytes  ca_bundle = 2;                           // 驗控制面用的 CA
  string assigned_machine_id = 3;
}

// 串流建立後的第一則 AgentMessage
message Register {
  string machine_id = 1;
  string hostname = 2;
  MachineSpec spec = 3;
  int32  agent_version = 4;
  string agent_semver = 5;                        // 人可讀，如 "1.4.2"
}
```

---

## 7. 串流訊息封套與服務定義

### 7.1 Lease 訊息

```proto
message LeaseRequest {
  string request_id = 1;
  string strategy = 2;
  string machine_id = 3;
  int32  requested_ttl_seconds = 4;
}

message LeaseRenew {
  string lease_id = 1;
  string strategy = 2;
}

message LeaseResponse {
  string request_id = 1;
  bool   granted = 2;
  string lease_id = 3;
  google.protobuf.Timestamp expires_at = 4;
  string deny_reason = 5;                         // 如 "held by machine M2 until T"
}
```

### 7.2 指令與事件

```proto
message TriggerRollback {
  string command_id = 1;
  string strategy = 2;
  string target_version = 3;                      // 空 = 回上一版
}

message DrainNow {
  string command_id = 1;
  string strategy = 2;
  int32  grace_seconds = 3;
}

message Ack {
  string command_id = 1;
}

message Nack {
  string in_reply_to = 1;                         // 被拒的 message_id / command_id
  string reason = 2;                              // 如 "UnknownCommand" | "UnsupportedInThisAgentVersion"
  int32  agent_version = 3;
}

message Event {
  google.protobuf.Timestamp timestamp = 1;
  EventSeverity severity = 2;
  string strategy = 3;
  string reason = 4;                              // "DeployStarted" | "AutoRollback" | "CrashLoop" | "LeaseSuicide"
  string message = 5;
  map<string, string> details = 6;
}

enum EventSeverity {
  EVENT_SEVERITY_UNSPECIFIED = 0;
  EVENT_SEVERITY_INFO = 1;
  EVENT_SEVERITY_WARNING = 2;
  EVENT_SEVERITY_ERROR = 3;
}

message StatusReport {
  int64 observed_generation = 1;
  repeated StrategyAssignmentStatus assignments = 2;
}
```

### 7.3 封套與 AgentService

```proto
message AgentMessage {
  string message_id = 1;
  oneof payload {
    Register      register      = 2;
    Heartbeat     heartbeat     = 3;
    StatusReport  status_report = 4;
    Event         event         = 5;
    Nack          nack          = 6;
    LeaseRequest  lease_request = 7;
    LeaseRenew    lease_renew   = 8;
  }
}

message ControlMessage {
  string message_id = 1;
  oneof payload {
    DesiredState    desired_state    = 2;         // 主訊息：全量期望狀態快照
    TriggerRollback trigger_rollback = 3;         // 即時性優化，非真相來源
    DrainNow        drain_now        = 4;
    LeaseResponse   lease_response   = 5;
    Ack             ack              = 6;
  }
}

service AgentService {
  // Bootstrap 通道：token 換憑證。此 RPC 不要求 mTLS 客戶端憑證。
  rpc Enroll(EnrollRequest) returns (EnrollResponse);

  // 主連線：agent 出站發起的雙向串流，全程 mTLS。
  // 所有南向指令沿此串流回推——控制面永不對 agent 發起入站連線。
  rpc Connect(stream AgentMessage) returns (stream ControlMessage);
}
```

---

## 8. 對人 API（控制面 ↔ Trader，HTTP/JSON via Connect）

這是與 AgentService **不同性質**的連線：低頻、瀏覽器/CLI、需可 curl。用 Connect 讓同一份 proto 同時服務 gRPC、gRPC-Web、純 HTTP/JSON。寫路徑一律經控制面——控制面驗證後更新 spec、遞增 `generation`、沿串流推新 DesiredState 給對應 agent。

```proto
message Machine {
  ObjectMeta metadata = 1;
  MachineSpec spec = 2;
  MachineResources last_resources = 3;
  bool reachable = 4;
  int32 agent_version = 5;
  google.protobuf.Timestamp last_heartbeat = 6;
}

message DeployRequest {
  string machine_id = 1;
  string strategy = 2;
  string artifact_version = 3;                    // 要部署的版本
  string config_version = 4;                      // 選填；空 = 沿用現配置
}
message DeployResponse {
  int64 generation = 1;                           // 產生的新 spec generation，供前端追蹤收斂
}

message RollbackRequest {
  string machine_id = 1;
  string strategy = 2;
  string target_version = 3;                      // 空 = 上一版
}
message RollbackResponse {
  int64 generation = 1;
}

message SetScheduleRequest {
  string machine_id = 1;
  string strategy = 2;
  repeated CronSchedule schedules = 3;            // 全量覆蓋該策略的 cron
}
message SetScheduleResponse {
  int64 generation = 1;
}

message AuditEntry {
  google.protobuf.Timestamp timestamp = 1;
  string actor = 2;                               // 誰
  string action = 3;                              // "Deploy" | "Rollback" | "ConfigChange"
  string machine_id = 4;
  string strategy = 5;
  string from_version = 6;
  string to_version = 7;
}

service ControlPlaneService {
  rpc ListMachines(ListMachinesRequest) returns (ListMachinesResponse);
  rpc GetMachine(GetMachineRequest) returns (Machine);

  rpc Deploy(DeployRequest) returns (DeployResponse);
  rpc Rollback(RollbackRequest) returns (RollbackResponse);
  rpc SetSchedule(SetScheduleRequest) returns (SetScheduleResponse);

  // UI 實時面板：機器狀態與部署進度推送（gRPC-Web streaming / SSE）
  rpc WatchMachine(GetMachineRequest) returns (stream MachineStatusEvent);

  rpc ListAudit(ListAuditRequest) returns (ListAuditResponse);
}
```

（`ListMachinesRequest` / `MachineStatusEvent` 等分頁與事件訊息此處省略，屬機械性定義。）

---

## 9. Proto 演進紀律

自我更新意味著系統永遠混版本：控制面 vX 同時服務 agent vN、vN-1、vN-2。相容性是協議紀律，不是臨時應對。

**硬規則**：

- **只做加法**：新增欄位用新號；永不改既有欄位的號或語義。
- **刪除即保留**：欲廢棄的欄位改標 `reserved`，號與名都保留，禁止重用。

  ```proto
  message StrategyAssignmentSpec {
    reserved 11, 12;
    reserved "old_field_name";
    // ...
  }
  ```
- **enum 只追加**：新值加在尾端；`*_UNSPECIFIED`(0) 永不移除。對端收到不認識的 enum 值須當作 `UNSPECIFIED` 處理，不可崩潰。
- **更新順序固定**：永遠**先更控制面、後更 agent**。控制面向後相容舊 agent 容易，反過來不可能。
- **顯式降級**：控制面對不支援某功能的舊 agent（由 `Heartbeat.agent_version` 判斷）拒絕下發相關 spec，並在 UI 標示「agent 過舊」；不可假設所有 agent 都懂新欄位。
- **未知即 Nack**：agent 收到 oneof 中不認識的 payload（proto 反序列化為空 oneof）時回 `Nack{reason:"UnknownCommand"}`，控制面據此得知該指令未被執行。

---

## 10. 關鍵訊息流

### 10.1 Enrollment → 首次收斂

```
Agent                          Control Plane
  │  Enroll(token, csr, spec)        │
  │─────────────────────────────────▶│  驗 token、簽 cert
  │  EnrollResponse(cert, ca)        │
  │◀─────────────────────────────────│
  │                                  │
  │  Connect: AgentMessage{Register} │  （mTLS）
  │─────────────────────────────────▶│  建立 machine 記錄
  │  ControlMessage{DesiredState g=1}│  推當前期望狀態
  │◀─────────────────────────────────│
  │  diff 本地 ∅ vs spec → 收斂       │
  │  AgentMessage{StatusReport,       │
  │    observed_generation=1}         │
  │─────────────────────────────────▶│  比對 g==observed → 收斂完成
```

### 10.2 發佈（人觸發 → 收斂 → 追蹤）

```
Trader ──Deploy(v42)──▶ Control Plane
                          │ 寫 spec.artifact=v42, generation++→g=1235
                          │ 寫 audit(actor, Deploy, v41→v42)
                          │ 沿串流推 ControlMessage{DesiredState g=1235}
                          ▼
                        Agent 收斂迴圈:
                          PENDING→DOWNLOADING→VERIFYING→DRAINING
                          →SWITCHING(symlink)→STARTING→HEALTH_CHECKING→HEALTHY
                          │ 每次轉移送 StatusReport{phase, observed_generation=1235}
                          ▼
                        Control Plane 更新 status；UI 經 WatchMachine 實時顯示卡在哪步
```

### 10.3 自動回滾

```
Agent（HEALTH_CHECKING 中，health_window 內崩潰 > max_crashes）:
  │ symlink 切回 v41、重啟
  │ AgentMessage{Event, reason:"AutoRollback", severity:ERROR}
  │ AgentMessage{StatusReport, phase:ROLLED_BACK, running_artifact:v41}
  ▼
Control Plane 標記 v42 為 bad（後續收斂不再拉起）、告警
```

### 10.4 Fencing lease（防雙開）

```
新機器 M2 欲啟動策略 S（M1 疑似網路分區）:
  M2 ──AgentMessage{LeaseRequest(S, ttl)}──▶ Control Plane
                                             │ 檢查 S 的現有 lease
                                             │  ├ M1 lease 未過期 → LeaseResponse{granted:false,
                                             │  │                    deny_reason:"held by M1 until T"}
                                             │  │  → M2 不啟動 S
                                             │  └ M1 lease 已過期 → LeaseResponse{granted:true, lease_id, expires_at}
                                             │                    → M2 啟動 S，並定期 LeaseRenew
  同時 M1（若其實還活著）續 lease 失敗 → 策略自殺（LeaseSuicide 事件）
```

### 10.5 定期 resync（對抗靜默偏差）

```
即使無 spec 變更，控制面每 N 分鐘無條件推一次 DesiredState{當前 generation}。
Agent 用 generation 判斷是否有更新；即便相同也重新 diff 一次實際狀態，
修正「串流沒斷但訊息悄悄丟」造成的偏差（k8s informer 標準防禦）。
```

---

## 11. 檔案佈局建議

```
proto/
  strategyplatform/v1/
    common.proto        # §2 通用型別、制品、資源、cron、lease
    spec.proto          # §3 DesiredState / StrategyAssignmentSpec
    status.proto        # §4 DeployPhase / StrategyAssignmentStatus
    telemetry.proto     # §5 Heartbeat / MachineResources / ProcessMetrics
    enrollment.proto    # §6 Enroll / Register / MachineSpec
    agent_service.proto # §7 AgentMessage / ControlMessage / service AgentService
    control_service.proto # §8 對人 API / service ControlPlaneService
buf.yaml
buf.gen.yaml            # 生成 Go（agent+控制面）與 TypeScript（Svelte 前端）
```

建議用 [buf](https://buf.build) 管理：`buf lint` 強制風格、`buf breaking` 在 CI 擋掉違反 §9 的破壞性變更、`buf generate` 一次產出 Go 與 TS 客戶端。破壞性變更防護對「協議是憲法」這件事是機械化的保險。

---

## 附錄：欄位號分配慣例

- 每個 message 的 `1` 號留給最常存取/最穩定的識別欄位（`strategy`、`machine_id`、`generation`）。
- 封套 message（`AgentMessage`/`ControlMessage`）的 `1` 號固定為 `message_id`，`2` 號起為 oneof payload。
- oneof 內新增 payload 一律取當前最大號 +1，永不插空號。
- 預留 `reserved` 區段時連同欄位名一起保留，防止語義重用。
