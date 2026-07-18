# Strategon

An internal platform for registering trading machines and publishing/supervising
strategy processes, built on **level-triggered desired-state convergence**
(kubelet-inspired): publish, rollback, and disaster recovery are unified into one
mechanism — change the desired state and let the agent converge.

Design docs: `ARCHITECTURE.md`, `PROTOCOL.md`, `RECONCILER.md`, `SAFETY.md`,
`FRONTEND.md`.

## Status

Foundation (agent reconciler + agent stream) plus the **human API and Svelte UI**
(`FRONTEND.md`): Connect `ControlPlaneService`, `StrategyView` join, deploy-phase
tracker, fleet panel.

```mermaid
flowchart LR
  UI["Svelte UI / curl"]
  Human["ControlPlaneService\n:8081 HTTP/JSON"]
  AgentSvc["AgentService\n:8080 stream"]
  Agent["Agent reconciler"]
  Proc["Strategy process"]
  UI -->|"unary + WatchMachine"| Human
  Human -->|"DesiredState push"| AgentSvc
  Agent -->|"outbound Connect"| AgentSvc
  Agent --> Proc
```

### Implemented

- **Protocol** (`proto/strategyplatform/v1`) — single schema for agent, control
  plane, and frontend. Generated Go under `gen/`, TypeScript under
  `web/src/lib/gen/`.
- **Agent** — level-triggered reconciler, exec driver (setsid/pidfd), supervisor,
  artifact layout, stream client.
- **Control plane**
  - `store` — in-memory spec/status/audit + artifact catalog + change hub
  - `grpcstream` — `AgentService` bidi stream
  - `api` — `ControlPlaneService`: List/Get/Deploy/Rollback/SetSchedule/
    WatchMachine/ListAudit/RegisterArtifact, with `StrategyView` join and
    `converged` computed server-side
- **Frontend** (`web/`) — SvelteKit + Connect-ES: fleet overview, machine detail
  (WatchMachine), **deploy phase tracker** (`/machines/:id/:strat`), deploy form,
  schedules/audit placeholders

## Requirements

- Go 1.22+ (Linux agent: `pidfd` needs kernel ≥ 5.3)
- Node 22 + pnpm (frontend)
- `make tools` for proto regeneration (`buf`, protoc plugins); frontend codegen
  also needs `web/node_modules/.bin/protoc-gen-es` (`make web-install`)

## Build & test

```bash
make build
make test
make lint
make generate          # Go + TS (requires make tools && make web-install)
make web-check         # svelte-check
```

## Run locally

### 1. Control plane (two ports)

```bash
go run ./cmd/controlplane \
  --agent-addr :8080 \
  --human-addr 127.0.0.1:8081
```

- `:8080` — `AgentService` (agents)
- `127.0.0.1:8081` — `ControlPlaneService` (UI/CLI; loopback, no auth)

### 2. Agent

```bash
go run ./cmd/agent \
  --control-plane http://127.0.0.1:8080 \
  --machine-id m1 \
  --base /tmp/strategon-m1
```

### 3. Frontend

```bash
cd web && pnpm i && pnpm run dev -- --host 127.0.0.1 --port 5173
```

Open http://127.0.0.1:5173 — Fleet → machine → strategy for the live phase
tracker.

### 4. curl the human API

```bash
# Register an artifact, then deploy
printf '#!/bin/sh\nexec sleep 300\n' > /tmp/strat.sh && chmod +x /tmp/strat.sh
DIGEST="sha256:$(sha256sum /tmp/strat.sh | cut -d' ' -f1)"

curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/RegisterArtifact \
  -H 'Content-Type: application/json' \
  -d "{\"artifact\":{\"name\":\"s\",\"version\":\"v1\",\"digest\":\"$DIGEST\",\"uri\":\"file:///tmp/strat.sh\",\"type\":\"ARTIFACT_TYPE_BINARY\"}}"

curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/Deploy \
  -H 'Content-Type: application/json' \
  -d '{"machineId":"m1","strategy":"s","artifactVersion":"v1"}'
# → {"generation":"1"}

curl -N -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/WatchMachine \
  -H 'Content-Type: application/json' \
  -d '{"machineId":"m1"}'
```

## Roadmap (still deferred)

- Artifacts/S3 + Postgres store
- Cron local executor (UI writes spec today; agent does not run it yet)
- Fencing lease + safety hardening (`SAFETY.md`)
- mTLS enrollment + SSO/authz on the human API
- Agent self-update + DR drills
