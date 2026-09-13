# Declarative NATS cluster

A NATS cluster is not a Strategon concept. It is an `AssignmentSet` — N
members, each an assignment named `member.name`, rolled `maxUnavailable` at
a time — plus a manifest that says what NATS wants. `spec.strategy` is the
catalog family (`nats` binary / `nats-config`). Nothing in the control plane
knows the string `NATS_SERVER_NAME`; `cluster.yaml` does.

Apply the document. The control plane **only persists the set object** — it
does not write member assignments on apply. A rolling controller later
writes one `member.name` assignment at a time. Same-host members are
allowed; see `single-host.yaml`.

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

# Lab: three nodes on one agent (ports 4222/4223/4224).
# strategon apply -f examples/nats/single-host.yaml
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

Machines listed in `spec.members[].machine` must already be registered
(agent connected at least once). The same machine may appear more than
once when `member.name` values differ.

## Per-member config

NATS shares one `nats-config` and differentiates via env. A workload whose
config file itself must differ (Redis `maxmemory` that cannot live in env)
binds a catalog name per member. `spec.config` and `members[].config` use
the same placeholders as the template except `${peers}`. An explicit name
does not fall back to `{strategy}-config`.

```yaml
spec:
  strategy: redis
  artifactVersion: v7
  config: ${member.name}-config   # redis-m1-config, redis-m2-config
  configVersion: v1
  members:
    - { machine: m1, name: redis-m1 }
    - { machine: m2, name: redis-m2, configVersion: v2 }  # this member only
    - { machine: m3, name: redis-m3, config: redis-m3-extra }  # different artifact
```

Omit both `config` fields to keep today's `{artifact}-config` then
`{strategy}-config` lookup. `configVersion: latest` is rejected.

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

Human `Deploy` / `ApplyAssignment` / `Rollback` of a **member name**
(`nats-m1`) onto that machine is rejected while the set owns the slot.
`Deploy nats` (the catalog family) is rejected only until
`status.assignment_key` flips to `member`. After that, `nats` is an
ordinary strategy name again.

`member.name` is the assignment slot (`<base>/<name>`; process cwd is
`<base>/<name>/work`). The agent does not share a
blob cache across names: three members on one host unpack the artifact
three times. Renaming a member is recreate (new empty dir; undeploy does
not delete the old one). To apply a human slot whose name is not the
artifact name, set `spec.artifact` on `kind: StrategyAssignment`.

## Edit `cluster.yaml`

Replace `10.0.0.1` / `m1` with your machines before applying. Ports default
to 4222 / 6222 / 8222 when omitted.
