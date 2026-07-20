# 策略發佈平台 — CI/CD 規格

> 版本 v0.1 · 聚焦迭代
>
> 承接整個系統的部署模型、VERSIONING、DEPLOYMENT_MODEL,以及 main 分支現況(有 Postgres store + migration、agent/CP 皆 Go、SvelteKit 前端)。
>
> 定義三層 CI/CD:test / build / deploy。核心約束:守住「CP 與 agent 皆為單一靜態二進位」的極簡哲學,CI/CD 立起「平台本身」,平台負責「agent 與策略」的鋪開——各司其職。

---

## 0. 分層總覽與職責邊界

| 層 | 觸發 | 產出/動作 | 本規格 |
|---|---|---|---|
| 1. Test | 每次提交 | unit test(integration 寫但暫不跑) | §2 |
| 2. Build | merge 到 main / 打 tag | CP 鏡像+二進位、frontend、agent 二進位 | §3 |
| 3. Deploy | tag 發布 | **只部署 CP + frontend** 到自架機器 | §4 |

**關鍵邊界(必守)**:第 3 層**只部署 CP + frontend 這個基礎設施**。以下三種「部署」不歸 CI/CD:
- **agent 分發到交易機器** → 走 bootstrap(mTLS enrollment)+ 平台自升級(`desired_agent_version` + canary),**不是 CI SSH 覆蓋二進位**。
- **策略部署** → 走平台的 `SetDeployment`,是平台功能,不是 CI。
- **PG 生命週期** → PG 是獨立底座,預先架好/託管,不歸這條 CI/CD(§4.3)。

把這三種混進 CI 第 3 層,會繞過 mTLS/enrollment/canary,並讓 pipeline 語義混亂。CI 立起平台,平台鋪開 agent 與策略。

---

## 1. 前置約束(貫穿三層)

- **CP 與 agent 皆 `CGO_ENABLED=0` 純靜態編譯**。這是三件事的共同前提:(a) 在 x86 runner 上交叉編譯 arm 二進位(帶 cgo 則交叉編譯是噩夢);(b) 兩階段 build 的 runtime 用 `scratch`(無 libc);(c) 真正 `curl+chmod` 即跑的靜態二進位。**確認代碼無 cgo 依賴**(純 Go pgx、純 Go crypto 皆可),且此約束需長期守住——別不慎引入帶 cgo 的庫。
- **版本注入(VERSIONING)**:所有產物 ldflags 注入 `buildinfo.Version`,tag build 時版本 = tag,**所有產物版本一致**(同一 git tag)。
- **frontend embed 進 CP**:CP 二進位用 `//go:embed` 嵌入 SvelteKit 靜態資源,自己 serve UI。**build 順序依賴**:先 `pnpm build`(出靜態資源)→ 再 `go build`(embed)。順序反了會嵌到空/舊資源。

---

## 2. 第一層:Test

### 2.1 只跑 unit test

- 每次提交(push / PR)跑 unit test。快(秒級)、無外部依賴、擋住大部分錯誤。
- **integration test 寫但 CI 暫不跑**:用 build tag 標記(`//go:build integration`),保留本地/手動可跑,待 CI 環境成熟(PG service container、確認 runner 可跑特權測試)再接入。**「CI 不跑」≠「不寫」**——pidfd/cgroup/store/差分計算這類只有真環境現形的問題,測試該寫,只是暫不自動跑。

### 2.2 unit / integration 分類紀律(現在就分)

- **unit**:純記憶體、無外部依賴 → CI 必跑。
- **integration**(build tag `integration`):碰 PG、碰 `/proc`、碰 fork 進程、碰 cgroup → 暫不跑。
- 現在就用 build tag 分好,否則測試混在一起無法「只跑 unit」。

### 2.3 CI 步驟

```yaml
test:
  runs-on: ubuntu-latest
  steps:
    - checkout
    - setup-go
    - run: go test ./...            # 預設不含 integration tag,只跑 unit
    - run: go vet ./...
    # (未來)integration job:起 postgres service container + go test -tags=integration
```

---

## 3. 第二層:Build

### 3.1 產物矩陣

| 產物 | 形態 | 架構 | 放哪 | 用途 |
|---|---|---|---|---|
| CP | 鏡像(兩階段, scratch runtime) | x86 + arm(buildx multi-arch) | ghcr | 容器部署 |
| CP | 二進位(靜態, embed frontend) | x86 + arm | GitHub Release | systemd 部署 |
| frontend | 鏡像(nginx + 靜態) | x86 + arm(可選) | ghcr | 前後端分離的容器部署(可選) |
| agent | 二進位(靜態) | x86 + arm | GitHub Release | bootstrap 到交易機器 |

> frontend 因 embed 進 CP,二進位部署不需要獨立 frontend 產物;獨立 frontend 鏡像僅在你想前後端分離伸縮的容器部署時用,否則可省(CP 鏡像也 embed 了 UI)。

### 3.2 觸發時機:預覽 vs 正式,分開

- **merge 到 main**:打**預覽產物**,tag `main-<short-sha>` / `latest-dev`。用於測試/預覽,非正式發布。
- **打 tag(如 `v1.4.0`)**:打**正式發布產物**,版本 = tag,鏡像 tag = `v1.4.0`,二進位版本注入 `v1.4.0`,寫進 GitHub Release。**只有 tag 產物可部署到生產**(呼應 DEPLOYMENT_MODEL:正式產物由 CI 在源頭算 digest 並可註冊進平台)。

不把「每次 merge 的中間產物」當正式版部署——tag 是「我確認這是發布」的顯式動作。

### 3.3 CP 兩階段 Dockerfile(multi-arch)

```dockerfile
# --- stage 1: frontend build ---
FROM node:20 AS fe
WORKDIR /web
COPY web/ .
RUN corepack enable && pnpm install --frozen-lockfile && pnpm build
# 產出 web/build(靜態資源)

# --- stage 2: go build (embed frontend) ---
FROM golang:1.xx AS builder
WORKDIR /src
COPY . .
COPY --from=fe /web/build ./internal/webassets/dist   # embed 目標目錄
ARG VERSION COMMIT BUILD_TIME TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build \
    -ldflags "-X .../buildinfo.Version=$VERSION -X .../buildinfo.CommitHash=$COMMIT -X .../buildinfo.BuildTime=$BUILD_TIME" \
    -o /out/controlplane ./cmd/controlplane

# --- stage 3: runtime (scratch, 靜態二進位) ---
FROM scratch
COPY --from=builder /out/controlplane /controlplane
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/  # 若需外連 TLS
ENTRYPOINT ["/controlplane"]
```

`docker buildx build --platform linux/amd64,linux/arm64 --push -t ghcr.io/.../controlplane:$VERSION`,一條命令出雙架構推 ghcr。`TARGETARCH` 由 buildx 注入。

### 3.4 二進位交叉編譯(CP + agent)

```makefile
# 先 build frontend(CP embed 需要)
web-build:
	cd web && pnpm install --frozen-lockfile && pnpm build && cp -r build ../internal/webassets/dist

# CP 雙架構(embed frontend)
cp-binaries: web-build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/controlplane-linux-amd64 ./cmd/controlplane
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/controlplane-linux-arm64 ./cmd/controlplane

# agent 雙架構
agent-binaries:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/agent-linux-amd64 ./cmd/agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/agent-linux-arm64 ./cmd/agent
```

`LDFLAGS` 同 VERSIONING §3(git describe + commit + UTC build time)。交叉編譯免費(純 Go + CGO_ENABLED=0),一個 x86 runner 出兩個架構。

---

## 4. 第三層:Deploy(只部署 CP + frontend)

### 4.1 部署形態:二進位(systemd)為主

因 CP 是 embed 了 frontend 的單一靜態二進位,主部署路徑用 **systemd 二進位**(和 agent 同哲學,機器不需容器 runtime):

```
1. (migration 先行,§4.2)
2. 機器 pull 對應架構的 CP 二進位(GitHub Release)
3. 替換 /opt/strategon/controlplane,systemctl restart strategon-cp
4. CP 起來:serve gRPC(agent)+ HTTP/JSON API + embed 的 UI,連既有 PG
```

容器部署(CP 鏡像)作為替代形態,給有容器環境的部署;二者產物在第二層都出,部署選一種。

### 4.2 Migration 在 deploy 之前跑 + expand-only 紀律(關鍵)

**Migration 作為 deploy 的前置步驟,在替換 CP 之前跑,且必須 expand-only。**

- **為何前置而非 CP 啟動時跑**:CP 啟動變純粹(不承擔改 schema);多 CP 實例啟動無 migration 競爭;migration 失敗則 deploy 停在此步,CP 不被替換,不會有「新 CP 起了但 schema 沒遷成」的半吊子。
- **為何必須 expand-only**:「先 migration 後換 CP」的順序,在「schema 已改、舊 CP 還在跑」的窗口裡,舊 CP 面對新 schema。若 migration 是破壞性的(刪列/改列),舊 CP 讀不懂會崩。**紀律:每個 migration 只做加法(加列/加表),破壞性變更拆成兩次發布——先加(這次)、確認新 CP 全上線不回滾後,下次才刪(contract)。** 現有 `0005_resource_samples.sql`(純加表加列)是正確示範。
- **在哪跑**:在目標機器上作為前置步驟跑(如 CP 二進位的 `migrate` 子命令),**不把生產 DB 憑證給 GitHub Actions**——呼應「不給 CI 過多生產訪問權」。

```
deploy 流程:
  pull CP 二進位 → 跑 migration(./controlplane migrate,expand-only)
    → 成功?→ 替換二進位 + systemctl restart
    → 失敗?→ 停止,不替換 CP(舊 CP 繼續跑,schema 未變或只多了沒用到的加法)
```

### 4.3 PG 是獨立底座,不歸 CI

- PG 預先架好或用託管服務,**其部署/升級不在這條 CI/CD**。
- CP 每次發布替換,PG 是長期穩定底座,兩者節奏完全不同,不可混在一起。
- CP 無狀態(真相在 PG),所以 CP 可隨意替換重啟;PG 有狀態,獨立謹慎運維。

### 4.4 重啟策略:起步用簡單重啟

- CP 無狀態,理論可 blue-green 零停機,但需 LB/流量切換,複雜。
- **起步用簡單重啟**(停舊起新,幾秒空窗)。可接受,因為:agent 是出站+重連(串流斷線不影響策略);lease TTL 顯著大於 CP 重啟時間(續約重試幾次即接上,策略不自殺——見 SAFETY 的 TTL 選型)。
- 規模大到重啟空窗不可接受時再上 blue-green(那時 migration 的 expand-only 紀律正好也是 blue-green 的前提)。

### 4.5 觸達自架機器:pull 模式 / self-hosted runner

CI(GitHub Actions,在 GitHub 雲)部署到自架機器,用**出站優於入站**:
- **pull 模式(推薦)**:CI 只推產物到 ghcr/Release;機器上一個 systemd timer / 輕量 agent 定期拉最新版並更新。CI 不主動連機器,機器不為 CI 開入站口。
- **或 self-hosted runner**:在你機器上跑一個 GitHub Actions runner,它出站連 GitHub 取任務。
- 二者都避免「為 CI 開入站 SSH」,與整個系統「出站優於入站」哲學一致。

---

## 5. Action Plan

1. **確認 `CGO_ENABLED=0` 可編譯**(CP + agent):檢查無 cgo 依賴;若有,替換成純 Go 實作。這是一切的前提。
2. **buildinfo + ldflags**(若 VERSIONING 已做則復用):確保 CP + agent 都注入版本。
3. **frontend embed**:`//go:embed` SvelteKit 靜態資源進 CP,CP 加 serve 靜態 UI 的能力;Makefile 定 `web-build` → `go build` 順序。
4. **第一層 test workflow**:unit test + vet,build tag 分好 unit/integration。
5. **第二層 build**:
   - CP 兩階段 Dockerfile(fe build → go embed build → scratch),buildx multi-arch 推 ghcr。
   - CP + agent 雙架構二進位交叉編譯,推 GitHub Release。
   - 觸發:merge→預覽 tag,git tag→正式 tag。
6. **CP migrate 子命令**:`./controlplane migrate` 跑 migration(expand-only),供 deploy 前置。
7. **第三層 deploy**:pull 模式部署 CP 二進位(migration 先行 → 替換 → restart);PG 獨立;簡單重啟。
8. **確立 migration expand-only 紀律**:文檔化,CI 可加 `buf`-style 的 migration 破壞性檢查(可選,類比 PROTOCOL 的 breaking check)。

---

## 6. 邊界(明確不做)

- **不部署 agent / 不部署策略**:各走 bootstrap+自升級 / SetDeployment,不歸 CI(§0)。
- **不管理 PG 生命週期**:獨立底座(§4.3)。
- **CI 暫不跑 integration test**:寫但不自動跑(§2.1)。
- **不給 CI 生產 DB 憑證**:migration 在目標機器上跑(§4.2)。
- **不為 CI 開入站**:pull / self-hosted runner(§4.5)。
- **不做 blue-green(起步)**:簡單重啟,TTL 吸收(§4.4)。

---

## 7. 驗收

- 每次提交跑 unit test + vet;integration test 存在(build tag)但 CI 不跑。
- 打 tag → 產出:CP multi-arch 鏡像(ghcr)+ CP 雙架構二進位 + agent 雙架構二進位(Release),全部版本 = tag 且一致,CP 二進位 embed 了 UI。
- 部署 CP:migration 先行(expand-only)→ 替換二進位 → restart;單一 CP 二進位即 serve gRPC + API + UI;連既有 PG;簡單重啟不影響 agent 與策略(TTL 吸收)。
- agent 分發、策略部署、PG 運維均**不**在此 pipeline——CI 立起平台,平台鋪開其餘。
- 應用產物皆 `CGO_ENABLED=0` 靜態,可 `curl+chmod` 部署;鏡像用 scratch 極小。
