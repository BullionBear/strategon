# Declarative NATS cluster

A NATS cluster is not a Strategon concept. It is an `AssignmentSet` — N
machines running one strategy, rolled `maxUnavailable` at a time — plus a
manifest that says what NATS wants. Nothing in the control plane knows the
string `NATS_SERVER_NAME`; `cluster.yaml` does.

Apply the document. The control plane **only persists the set object** — it
does not write per-machine assignments on apply. A rolling controller later
writes one `nats` assignment at a time.

## Where the NATS knowledge lives

| Concern | Expressed as |
|---|---|
| Per-member identity (`server_name`) | `template.env` + `${member.name}` |
| Ports | `members[].vars` + `${member.vars.client_port}` |
| Route list | `template.peers.format` → `--routes` in `template.args` |
| Config file | `-c ${CONFIG}` in `template.args` (the agent resolves `${CONFIG}`) |
| Readiness | `template.readiness.endpoint` → `/healthz` on the monitor port |

Routes ride on the command line because NATS cannot expand an environment
variable into a config array. A single-member set renders `${peers}` empty,
so the member starts with `--routes ""` — verified against nats-server
2.10.29, which keeps cluster mode and listens for route connections with no
peers to dial. The control plane renders args in place and never drops them;
what an empty value means is the workload's business.

To run something else that needs peers — a sharded feed handler, say — write a
different manifest. No server code changes.

```bash
# Token is required when the human API is not --auth-mode=none.
export STRATEGON_TOKEN=str_live_…
export STRATEGON_ADDR=http://127.0.0.1:8081

strategon apply -f examples/nats/cluster.yaml
strategon get assignmentset trading
strategon wait assignmentset trading --for=ready --timeout=5m
```

`apply` calls `ApplyAssignmentSet`. It does **not** loop `Deploy` on the client.
Re-applying the same file is a no-op (cluster generation does not bump).

## Register the binary and config

1. Register the NATS server binary as artifact `nats` (version must match
   `spec.artifactVersion`):

   ```bash
   DIGEST="sha256:$(sha256sum /path/to/nats-server | cut -d' ' -f1)"
   curl -sX POST "$STRATEGON_ADDR/strategyplatform.v1.ControlPlaneService/RegisterArtifact" \
     -H "Authorization: Bearer $STRATEGON_TOKEN" \
     -H 'Content-Type: application/json' \
     -d "{\"artifact\":{\"name\":\"nats\",\"version\":\"v2.10.24\",\"digest\":\"$DIGEST\",\"uri\":\"file:///path/to/nats-server\",\"type\":\"ARTIFACT_TYPE_BINARY\"}}"
   ```

2. Register this directory's `nats.conf` as `nats-config` (or
   `{artifact}-config`). The catalog resolves `configVersion` through
   `{artifact}-config`, then `{strategy}-config`:

   ```bash
   DIGEST="sha256:$(sha256sum examples/nats/nats.conf | cut -d' ' -f1)"
   curl -sX POST "$STRATEGON_ADDR/strategyplatform.v1.ControlPlaneService/RegisterArtifact" \
     -H "Authorization: Bearer $STRATEGON_TOKEN" \
     -H 'Content-Type: application/json' \
     -d "{\"artifact\":{\"name\":\"nats-config\",\"version\":\"v1\",\"digest\":\"$DIGEST\",\"uri\":\"file://$(pwd)/examples/nats/nats.conf\",\"type\":\"ARTIFACT_TYPE_BINARY\"}}"
   ```

Machines listed in `spec.servers[].machine` must already be registered
(agent connected at least once).

## Routes and `routeHost`

`nats.conf` has **no** `routes:` block. `$VAR` cannot expand into a NATS
config array, so the controller sets:

```
-c ${CONFIG} --routes nats://<other>:6222,...
```

`routeHost` is the address **peer members use to reach this node**. Use a
real, routable host or IP. `localhost` / `127.0.0.1` only works for a
single-host lab — other machines cannot join that route.

## Rolling and readiness

The controller writes assignments with:

- `enable_auto_rollback: true`
- a readiness probe on `http://127.0.0.1:<monitorPort>/healthz`
- `EnforceLeaseInterlock: false` (no fencing lease on NATS)

A node is ready only when the assignment is **converged and Ready=TRUE**.
The agent treats a configured probe as a hard health-window deadline: if
`/healthz` is not Ready before `waitReadySeconds`, it rolls back or fails
instead of marking HEALTHY. That is what makes a production roll safe.

Human `Deploy` / `ApplyAssignment` / `Rollback` of `nats` onto a member
machine is rejected (`FailedPrecondition`) while the cluster owns that
slot.

## Edit `cluster.yaml`

Replace `10.0.0.1` / `m1` with your machines before applying. Ports default
to 4222 / 6222 / 8222 when omitted.
