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

## Status

Under active development. APIs, storage, and ops paths will keep changing.

If you are interested in the project, feel free to contact
[eddy@lynxlinkage.com](mailto:eddy@lynxlinkage.com).
