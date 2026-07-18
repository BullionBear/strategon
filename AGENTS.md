# AGENTS.md

## Cursor Cloud specific instructions

Strategon is a Go backend project (no frontend yet). It has two runnable binaries plus
a proto/codegen toolchain. Standard commands live in the `Makefile` and `README.md`;
prefer those over duplicating.

### Services / binaries

- **Control plane** (`cmd/controlplane`): serves the `AgentService` bidi stream over
  h2c (plaintext HTTP/2) on `:8080`, plus a `/admin/assign` JSON stand-in and
  `/healthz`. Run: `go run ./cmd/controlplane --addr :8080`.
- **Agent** (`cmd/agent`): reconciler + outbound stream client. Requires `--machine-id`.
  Run: `go run ./cmd/agent --control-plane http://127.0.0.1:8080 --machine-id m1 --base /tmp/strategon-m1`.

The two talk to each other, so to exercise the "change desired state → agent converges"
flow end to end, start the control plane first, then the agent, then POST an assignment
to `/admin/assign` (see `README.md` "Run locally" for the exact curl + digest recipe).

### Lint / test / build

- Build/test: `make build` / `make test` (i.e. `go build ./...` / `go test ./...`).
- Lint here means **proto** lint: `make lint` runs `buf lint`. `buf` is installed into
  `$(go env GOPATH)/bin` by the update script. The `Makefile` prepends that dir to
  `PATH`, so `make lint` works even when `buf` is not otherwise on `PATH`; to call `buf`
  directly, use `"$(go env GOPATH)/bin/buf"` or add that dir to `PATH`.

### Non-obvious notes

- `make generate` (regenerating `gen/` from `proto/`) additionally needs the protoc
  plugins from `make tools` (`protoc-gen-go`, `protoc-gen-connect-go`). These are NOT
  installed by the update script because generated code is committed; only install them
  if you intend to regenerate.
- The agent's exec driver is Linux-only (`pidfd` needs kernel ≥ 5.3) and uses `setsid`
  + best-effort cgroup v2; the cloud VM satisfies this. cgroup confinement is disabled
  unless `--cgroup-root` is passed.
- Deploy lifecycle events (`DeployStarted`, `DeployHealthy`, etc.) are streamed to the
  control plane, so they show up in the **control-plane** logs, not the agent logs.
- Runtime artifacts land under the agent `--base` dir (e.g. `/tmp/strategon-m1/releases/<v>`);
  `/tmp/strategon-*` is gitignored.
- Most of the code currently lives on the `cursor/strategon-foundation-0de8` branch;
  `main` only contains a stub `README.md`. The update script guards on `go.mod` existing
  so it is a safe no-op on the empty `main` branch.
