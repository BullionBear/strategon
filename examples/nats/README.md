# Declarative NATS cluster

Apply a `NatsCluster` document. The control plane **only persists the cluster
object** — it does not write per-machine assignments on apply. A rolling
controller later writes one `nats` assignment at a time (`maxUnavailable`).

```bash
# Token is required when the human API is not --auth-mode=none.
export STRATEGON_TOKEN=str_live_…
export STRATEGON_ADDR=http://127.0.0.1:8081

strategon apply -f examples/nats/cluster.yaml
strategon get natscluster trading
strategon wait natscluster trading --for=ready --timeout=5m
```

`apply` calls `ApplyNatsCluster`. It does **not** loop `Deploy` on the client.
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
-c ${CONFIG} --routes nats-route://<other>:6222,...
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
