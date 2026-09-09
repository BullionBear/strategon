# Strategon

<p align="center">
  <img src="docs/logo.webp" alt="Strategon" width="128" />
</p>

Strategon is an experiment in applying Kubernetes ideas — desired state,
level-triggered reconciliation, and agent-driven convergence — to a narrower
problem: supervising **trading strategy processes** on real machines.

Instead of scheduling containers across a cluster, Strategon registers machines,
pushes a desired strategy assignment from a control plane, and lets each agent
converge: fetch the artifact, start or replace the process, report status, and
recover after restarts. Publish, rollback, and disaster recovery share that same
loop — change the desired state and wait for convergence.

The project is small on purpose: one control plane, one agent per host, and a
simple human API (plus an embedded UI) for deploy and observe.

For a map of the repository and control loop, see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

Minimum path: run a control plane in Docker, connect an agent, deploy a sample
process.

### 1. Control plane (Docker)

Build and run locally (in-memory store, no auth, no mTLS):

```bash
docker build -t strategon:local .
docker run --rm --name strategon-cp \
  -p 8080:8080 -p 8081:8081 \
  strategon:local \
  --agent-addr=0.0.0.0:8080 \
  --human-addr=0.0.0.0:8081 \
  --auth-mode=none
```

- `:8080` — agent stream (`AgentService`)
- `:8081` — human API + UI (`ControlPlaneService`)

Open http://127.0.0.1:8081 for the UI.

### 2. Agent

In another terminal, from this repo:

```bash
go run ./cmd/agent \
  --control-plane http://127.0.0.1:8080 \
  --machine-id m1 \
  --base /tmp/strategon-m1
```

The agent dials the control plane, registers as `m1`, and waits for desired
state.

### 3. Deploy an example process

Create a trivial strategy binary (a long-running shell script), register it, and
deploy it to `m1`:

```bash
printf '#!/bin/sh\necho "hello from strategon"; exec sleep 3600\n' > /tmp/hello.sh
chmod +x /tmp/hello.sh
DIGEST="sha256:$(sha256sum /tmp/hello.sh | cut -d' ' -f1)"

curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/RegisterArtifact \
  -H 'Content-Type: application/json' \
  -d "{\"artifact\":{\"name\":\"hello\",\"version\":\"v1\",\"digest\":\"$DIGEST\",\"uri\":\"file:///tmp/hello.sh\",\"type\":\"ARTIFACT_TYPE_BINARY\"}}"

curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/Deploy \
  -H 'Content-Type: application/json' \
  -d '{"machineId":"m1","strategy":"hello","artifactVersion":"v1"}'
```

The agent fetches `file:///tmp/hello.sh` from the local filesystem, starts it
under its release layout, and reports phase/status back to the control plane.
Watch the fleet UI or:

```bash
curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/GetMachine \
  -H 'Content-Type: application/json' \
  -d '{"machineId":"m1"}'
```

## OCI images (rootless)

The agent can run a `docker save` / OCI-layout archive without a Docker
daemon. Register it as `ARTIFACT_TYPE_OCI_IMAGE`. The control plane sets
`EXECUTION_DRIVER_OCI` from that type. The agent unpacks the archive, then
starts it in an unprivileged user namespace (single UID map, host network).

```bash
docker save my/strategy:v1 > /tmp/strategy-v1.tar
DIGEST="sha256:$(sha256sum /tmp/strategy-v1.tar | cut -d' ' -f1)"

# Presigned PUT (object store must be configured). Then register the s3:// URI.
UPLOAD=$(curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/CreateArtifactUpload \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"ml\",\"version\":\"v1\",\"digest\":\"$DIGEST\",\"type\":\"ARTIFACT_TYPE_OCI_IMAGE\"}")
PUT_URL=$(printf '%s' "$UPLOAD" | python3 -c 'import json,sys; print(json.load(sys.stdin)["putUrl"])')
S3_URI=$(printf '%s' "$UPLOAD" | python3 -c 'import json,sys; print(json.load(sys.stdin)["s3Uri"])')
curl -sS -X PUT -H 'Content-Type: application/octet-stream' --upload-file /tmp/strategy-v1.tar "$PUT_URL"

curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/RegisterArtifact \
  -H 'Content-Type: application/json' \
  -d "{\"artifact\":{\"name\":\"ml\",\"version\":\"v1\",\"digest\":\"$DIGEST\",\"uri\":\"$S3_URI\",\"type\":\"ARTIFACT_TYPE_OCI_IMAGE\"}}"
```

The Artifacts page can hash + PUT + register from the browser. That needs CORS on the S3
bucket. Production `deploy/seaweedfs/cors.json` allows only `https://s7n.lynkora.com`
(a leaked 15-minute presigned PUT must not be replayable from an arbitrary origin).
The local test stack applies `deploy/seaweedfs/cors.test.json` (`AllowedOrigins: *`)
instead — do not copy that wildcard into production. If you add another UI origin,
edit the production file; do not reopen `*`. Large uploads skip in-browser hashing
and expect a pasted `sha256sum`. If the browser PUT is blocked, the curl sequence
above still works.

Local / file URI (agent and file on the same machine) still works:

```bash
curl -sX POST http://127.0.0.1:8081/strategyplatform.v1.ControlPlaneService/RegisterArtifact \
  -H 'Content-Type: application/json' \
  -d "{\"artifact\":{\"name\":\"ml\",\"version\":\"v1\",\"digest\":\"$DIGEST\",\"uri\":\"file:///tmp/strategy-v1.tar\",\"type\":\"ARTIFACT_TYPE_OCI_IMAGE\"}}"
```

Requirements and limits:

- Host must allow unprivileged user namespaces. After enabling, restart the agent
  (capability is reported only at Register).
- Payload is PID 1: SIGTERM is ignored unless the process installs a handler;
  stop waits `stop_grace` then SIGKILL.
- cwd is `<base>/<strategy>/work` (not StrategyDir). Only `${CONFIG}` is
  valid in args; `${RELEASE_DIR}` and `${BINARY}` are rejected.
- No registry pull, no `/sys/fs/cgroup` inside the container, no per-run
  writable overlay. Old releases are GC'd (`--release-retention`, default 3);
  `Rollback` to a GC'd `target_version` re-downloads.

## Declarative multi-machine workloads

`strategon apply -f` sends an `AssignmentSet` document to `ApplyAssignmentSet`.
The control plane stores the object only; a rolling controller writes one
member assignment at a time (`maxUnavailable`), advancing only when the
in-flight member is converged **and** Ready.

An `AssignmentSet` is workload-agnostic. Per-member identity and any peer list
come from a template the manifest supplies:

```yaml
template:
  args: ["-c", "${CONFIG}", "--routes", "${peers}"]
  env: { NATS_SERVER_NAME: "${member.name}" }
  peers: { format: "nats://${peer.vars.route_host}:${peer.vars.cluster_port}" }
members:
  - { machine: m1, name: nats-m1, vars: { route_host: "10.0.0.1", cluster_port: "6222" } }
```

The control plane expands `${set.*}`, `${member.*}` and `${peers}`; `${CONFIG}`
is left for the agent. An unknown placeholder is rejected at apply time.

A NATS cluster is therefore a manifest, not a feature — see
[examples/nats/](examples/nats/). Running something else with the same shape
needs no server change.

```bash
go run ./cmd/strategon apply -f examples/nats/cluster.yaml
go run ./cmd/strategon wait assignmentset trading --for=ready
```

Safe production rolls need the readiness probe the manifest sets; addresses in
`vars` must be dialable by the other members.

## Status

Under active development. APIs, storage, and ops paths will keep changing.

If you are interested in the project, feel free to contact
[eddy@lynxlinkage.com](mailto:eddy@lynxlinkage.com).
