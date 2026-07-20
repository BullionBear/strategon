# Deployment

How the Strategon control plane runs in production. Companion to [CICD.md](CICD.md),
which specifies the pipeline; this describes what is actually deployed.

**Live:** <https://s7n.lynkora.com>

---

## 1. Topology

```
                     ┌─────────────────────────────────────────┐
  browser ──HTTPS──▶ │ Traefik (:80/:443, shared with portal,   │
                     │ autom, …) — Let's Encrypt resolver "le"  │
                     └────────────────────┬────────────────────┘
                                          │ docker network "web"
                                          ▼
                     ┌─────────────────────────────────────────┐
                     │ strategon-controlplane-1                │
                     │   :8081 UI + human API + /auth  ◀ Traefik│
                     │   :8080 AgentService  ◀── mTLS, published │
                     │        on 100.108.10.2:8443 (Tailscale)  │
                     │   one static binary, scratch image       │
                     └──────────┬─────────────────────┬────────┘
                                │                     │ private-CA mTLS
                                │                     ▼
                                │        ┌──────────────────────────┐
                                │        │ agent "yite"             │
                                │        │ 100.65.26.119, unprivil. │
                                │        └──────────────────────────┘
                                ▼
                     ┌─────────────────────────────────────────┐
                     │ Postgres 172.238.24.139:5432/strategon   │
                     │ separate host, not managed by this repo  │
                     └─────────────────────────────────────────┘
```

The control plane is a single `CGO_ENABLED=0` binary with the SvelteKit SPA
compiled in via `//go:embed`, so one container serves the UI, the human JSON API
and the agent gRPC endpoint. There is no separate frontend service and no
reverse proxy inside the stack.

**Host:** `139.162.74.23` (Ubuntu 24.04, 2 vCPU, 3.8 GB RAM). Shared with
`portal`, `autom`, `statarb` and a Postgres container. The control plane idles
at roughly 20 MB RSS, so headroom is not a concern at present.

---

## 2. What is and isn't deployed here

Per [CICD.md §0](CICD.md), this pipeline stands up **the platform only**:

| Deployed by CI | Not deployed by CI |
|---|---|
| Control plane + embedded UI | **Agents** — delivered by bootstrap (mTLS enrollment) and self-upgrade |
| | **Strategies** — deployed by the platform via `SetDeployment` |
| | **Postgres** — long-lived substrate, operated separately |

Routing these through CI would bypass mTLS enrollment and canary rollout.

---

## 3. Pipeline

Two workflows, in `.github/workflows/`:

**`test.yml`** — every push to `main` and every PR.
Runs `go vet`, `go test ./...` (unit only; integration tests stay behind the
`integration` build tag), an arm64 cross-compile that guards the no-cgo
premise, plus `svelte-check` and a frontend build.

**`release.yml`** — on push to `main` and on `v*` tags.

| Trigger | Result |
|---|---|
| push to `main` | Preview image only: `ghcr.io/bullionbear/strategon:main-<sha>` and `:latest-dev`. Never deployed. |
| tag `vX.Y.Z` | Release image `:vX.Y.Z` + `:latest` (linux/amd64 + linux/arm64), static CP and agent binaries with SHA256SUMS attached to a GitHub Release, then deploy. |

Only tagged builds reach production — the tag is the explicit "this is a
release" act.

### Cutting a release

```bash
git tag v0.2.0 && git push origin v0.2.0
```

The deploy job then SSHes to the host, rewrites `STRATEGON_VERSION` in
`.env`, pulls, restarts, and polls `https://s7n.lynkora.com/healthz` for up to
150 s — dumping container logs and failing if the endpoint never comes up.
Because it probes through Traefik, a passing deploy also proves routing and the
certificate survived.

---

## 4. Server layout

```
/opt/strategon/deploy/
├── docker-compose.yml   # synced from deploy/ in this repo
└── .env                 # secrets, chmod 600, NEVER overwritten by CI
```

CI only rewrites the `STRATEGON_VERSION` line in `.env`. Changes to
`docker-compose.yml` are **not** synced automatically — copy it by hand:

```bash
scp deploy/docker-compose.yml root@139.162.74.23:/opt/strategon/deploy/
ssh root@139.162.74.23 'cd /opt/strategon/deploy && docker compose up -d'
```

### `.env` contents

| Key | Notes |
|---|---|
| `STRATEGON_VERSION` | Image tag. Managed by CI. |
| `STRATEGON_DB_DSN` | `postgresql://bullionbear:…@172.238.24.139:5432/strategon` |
| `SESSION_SECRET` | Cookie HMAC key. Rotating it logs everyone out. `openssl rand -hex 32` |
| `DISCORD_CLIENT_ID` / `DISCORD_CLIENT_SECRET` | OAuth application |
| `DISCORD_GUILD_ID` | Login allowlist — see §5 |

Template: [`deploy/.env.example`](deploy/.env.example).

### GitHub secrets

`DEPLOY_SSH_KEY` (ed25519 private key), `DEPLOY_HOST`, `DEPLOY_KNOWN_HOSTS`.
The matching public key is in the host's `~/.ssh/authorized_keys`, commented
`github-actions-deploy@strategon`.

The host holds **no** registry credential. The deploy job logs the host into
GHCR with its own run-scoped `GITHUB_TOKEN` and the token expires with the run.

---

## 5. Access control — read this before widening anything

Authorization in the control plane is **flat**: any principal who logs in is a
full operator, able to deploy and kill strategies. There are no roles.

The only gate is Discord guild membership. `--discord-guild-id` restricts login
to members of guild `1297170481031548928`; the OAuth flow requests the `guilds`
scope and verifies membership against `/users/@me/guilds`, failing closed if
Discord errors. **Blanking `DISCORD_GUILD_ID` lets any Discord account on the
internet operate the platform** — the control plane logs a warning at startup
when it is unset.

Guild membership is checked at login, not per request. Someone removed from the
guild keeps any live session and any API token they already minted until those
expire — revoke tokens explicitly.

### Manual step: Discord Developer Portal

The redirect URI must be registered on the OAuth application, or login fails
with `invalid_redirect_uri`. This cannot be automated via the bot token:

> Discord Developer Portal → Application `1525825932999266404` → OAuth2 →
> Redirects → add `https://s7n.lynkora.com/auth/callback`

---

## 6. Agent endpoint and mTLS

`AgentService` is published on **`100.108.10.2:8443`** — the control-plane
host's Tailscale address only. It is not reachable on `139.162.74.23`; the
public interface exposes nothing but Traefik's 80/443.

Traefik is deliberately not in this path. It would terminate TLS and strip the
client certificate the control plane needs to verify, so the endpoint carries
its own private-CA TLS end to end and the control plane enforces
`RequireAndVerifyClientCert`. A connection without a client certificate fails
the handshake.

### The CA

Ed25519 root CA issued by `cmd/strategon-ca`. **`ca/ca-key.pem` is offline** —
it exists only on the operator's machine and must never reach a server. The
host holds the CA *certificate* (to verify agents) and its own server keypair,
in `/opt/strategon/tls/`, mounted read-only at `/tls`.

| Certificate | CN | SANs | Location |
|---|---|---|---|
| Root CA | `strategon-ca` | — | offline; cert only on hosts |
| Server | `control-plane` | `IP:100.108.10.2` | `/opt/strategon/tls/` on CP host |
| Client | `yite` | — | `~/strategon/tls/` on the agent host |

The server certificate's IP SAN pins it to the Tailscale address — reaching the
control plane by any other address fails verification.

Issuing another agent identity:

```bash
./bin/strategon-ca sign --ca ./ca/ --cn <machine-id> --out ./certs/<machine-id>/
```

The agent's machine ID defaults to its client-certificate CN, so the CN *is*
the machine identity. Nothing else authenticates an agent.

### Registered agents

Machines self-register on first connect — there is no separate registration
step or RPC.

| Machine | Region | Host | Runs as |
|---|---|---|---|
| `yite` | `tw` | `100.65.26.119` (Ubuntu 24.04, x86_64) | user `yite`, systemd user unit |

Region and zone are operator labels passed with `--region` / `--zone`; the host
cannot discover them. The fleet table groups by region and shows machines with
no region under "Unassigned".

Agent install layout on that host:

```
~/strategon/agent                 # static binary
~/strategon/tls/{cert,key,ca-cert}.pem
~/strategies/                     # --base, strategy release trees
~/.config/systemd/user/strategon-agent.service
```

```bash
systemctl --user status strategon-agent
journalctl --user -u strategon-agent -f
```

The unit runs **unprivileged**. The agent forks and supervises strategy
processes, so running it as root would hand every strategy root on that box.

Two consequences of that choice, both currently unresolved:

- **`loginctl enable-linger yite` must be set**, or the user-level systemd
  manager exits when the last session closes and the agent stops with it.
- **`--cgroup-root` is empty, so strategies run without cgroup confinement.**
  Delegating a cgroup subtree to a non-root user needs a systemd drop-in. Worth
  doing before running anything real on that machine.

---

## 7. Migrations

Migrations are embedded in the binary and run at startup in
`store.NewPostgres`, each in its own transaction, recorded in
`schema_migrations`.

**Every migration must be expand-only** (add columns/tables; never drop or
retype). During a deploy the new container starts while the old one is still
serving, so the old binary meets the new schema. A destructive change breaks it
mid-swap. Split destructive changes across two releases: add now, remove only
after the new version is confirmed and won't be rolled back.

This differs from [CICD.md §4.2](CICD.md), which specifies migrations as a
pre-deploy step. With a single control-plane instance, startup migration is
equivalent and simpler — there is no concurrent-migration race to avoid. Revisit
if a second instance is ever added.

---

## 8. Operations

```bash
ssh root@139.162.74.23
cd /opt/strategon/deploy

docker compose ps
docker compose logs -f controlplane
docker compose restart controlplane
```

**Rollback** — retag to a known-good version:

```bash
sed -i 's|^STRATEGON_VERSION=.*|STRATEGON_VERSION=v0.1.0|' .env
docker compose up -d
```

Rollback is safe for code but **not for schema**: expand-only migrations mean an
older binary simply ignores newer columns. It cannot undo a migration.

Restarts are deliberately simple — stop old, start new, a few seconds of gap
([CICD.md §4.4](CICD.md)). Agents reconnect outbound and lease TTLs comfortably
exceed the window, so running strategies are unaffected.

### Troubleshooting

| Symptom | Check |
|---|---|
| 404 from Traefik | Container on the `web` network? `docker compose logs` for a crash loop. |
| Certificate errors | Traefik holds the ACME state: `docker logs traefik-traefik-1`. |
| `invalid_redirect_uri` | Redirect URI not registered — §5. |
| Login rejects a valid user | Not in guild `1297170481031548928`. |
| `UI not embedded in this build` | Binary built without `make web-build` staging `internal/webassets/dist`. |
| Blank page, 404s on `/_app/*` | Stale `dist/` embedded — rerun `make web-build`. |

---

## 9. Provisioned state

For rebuilding from scratch:

- **DNS** — `s7n.lynkora.com` A → `139.162.74.23`, TTL 3600, via the Hostinger
  API. Nameservers `hermes/artemis.dns-parking.com`.
- **Database** — `strategon` on `172.238.24.139:5432`, owned by `bullionbear`.
  Schema is created by the binary's own migrations on first start.
- **Traefik** — pre-existing and shared. Router `strategon`, entrypoint
  `websecure`, certresolver `le`, backend port 8081. No Traefik config was
  modified; everything is expressed as labels on the container.

### Known gaps

- The Hostinger API token lives in `credentials/` (gitignored). It is not used
  by any automation — DNS was a one-time manual step.
- Agent certificates carry no expiry-driven rotation process yet. Reissue with
  `strategon-ca sign` and restart the unit; there is no revocation list, so a
  compromised agent key means reissuing the CA and every certificate under it.
- `DISCORD_BOT_TOKEN` is not used anywhere. Guild membership is checked through
  the user's own OAuth grant, so no bot credential sits on the host.
- No backups are configured for the `strategon` database. The control plane is
  otherwise stateless — all durable state is in Postgres, so this is the one
  thing worth backing up.
