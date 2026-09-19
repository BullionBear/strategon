# Secret management

Status: implemented — v1 (Put/Get/List, apply refs, southbound resolve, CLI, `/secrets`)
Depends on: [Architecture](ARCHITECTURE.md)
Addresses: [issue #48](https://github.com/BullionBear/strategon/issues/48)

v1 lands an independent **SecretManagement** module on the deploy path.
Credentials live in a named Secret store. Assignment `env` and `member.vars`
stay `map<string, string>`. The public token on both sides is
`secret.<name>` (for example `secret.db-url`). Ciphertext never leaves the
store. Human read APIs never return plaintext. The only decrypt sink is the
southbound `DesiredState` clone, resolved live by name, then pushed to the
assigned agent.

This is not `valueFrom`, not an envelope pasted into the manifest, and not
an authorization model. Auth stays flat: a session or API token is still a
full operator.

---

## Why

Issue #48: env is a plain string map, persisted as protobuf, and
`ListAssignmentSets` / `GetAssignmentSet` echo it. A plane that needs
`DATABASE_URL` has nowhere safe to put it. The blast radius of a database
backup or a pasted list response is the production password.

Three things this design changes:

1. Named secrets are ciphertext at rest. Every human RPC that touches an
   assignment or a Secret returns `secret.<name>` (plus metadata), never
   plaintext, never ciphertext.
2. There is no decrypt / unwrap RPC.
3. The agent still receives ordinary plaintext env, because that is what
   `execve` requires. The agent is the last mile, not a new trust root.

It does not change who may apply, and it does not make unsealed values
illegal. Putting a password in env as ordinary text still works and is still
insecure. Credentials must go through SecretManagement; that is an operator
rule, not an admission default in v1.

A later change may reshape the store. The v1 contract is already a
SecretManagement participant in deploy, not a string-encoding hack waiting
to be replaced.

---

## Trust boundaries

| Place | After this design |
|-------|-------------------|
| Secret store | Ciphertext + metadata only |
| `assignment_sets.spec` / `assignments.spec` | `secret.<name>` or ordinary text |
| `GetAssignmentSet`, `ListAssignmentSets`, apply responses, CLI `get` | `secret.<name>` or ordinary text |
| `GetSecret` / `ListSecrets` | name, length, key id — never plaintext, never ciphertext |
| `GetMachine` / `WatchMachine` | Unchanged — `StrategyView` has no env |
| Audit `Detail` | Names, lengths, key id — never plaintext, never ciphertext |
| Control plane process | Holds `K`; sees plaintext only while building the southbound clone |
| Agent + payload | Plaintext env (required for injection) |
| Host `/opt/strategon/deploy/.env` | `K`, same class as `SESSION_SECRET` / S3 keys |

A token that can only call read APIs sees `secret.db-url`. A token that can
also `Apply*` can hang that same name on a machine the caller already
controls; SecretManagement will resolve onto that agent. That is the
intended decrypt sink, not a read-API leak. Splitting "may list" from
"may apply" is a separate auth ticket.

---

## Public contract

The process name stays the real env key. The value is either ordinary text
or a Secret reference:

```text
DATABASE_URL=secret.db-url
```

`secret.` is the reserved prefix. `db-url` is the Secret name. The same
string is what `PutSecret` returns, what List/Get Secret echo, and what
operators write in manifests and `member.vars`. There is one public token
on both sides. `$name` in discussion is a documentation placeholder, not
wire syntax — the wire form is `secret.db-url`, not `secret.$db-url`.

Do not encode secrecy in the key name (`STR_SECRET.DATABASE_URL=...`).
That forces a rename before `exec`, collides with template expansion, and
splits `DATABASE_URL` / `STR_SECRET.DATABASE_URL`.

Do not introduce `valueFrom`. Expand, `SetDeployment`, Apply, and existing
nats manifests already walk string values. `${member.vars.DB}` expands to
the same `secret.<name>` token; live resolve happens later on the
`DesiredState` clone.

A value that starts with `secret.` is a reference. Apply looks the name up
in SecretManagement. Unknown name, empty name, or a structurally broken
token is rejected. It does not need `K` when the module is up (existence
is metadata). To store the literal string `secret.db-url` as an env value,
put that literal in a Secret and reference it.

`strsec1` is not part of this contract. Ciphertext is store-internal.

---

## Secret store and wrap key

SecretManagement owns the named rows and the symmetric wrap key `K`.
`PutSecret` is an RPC, so `K` never leaves the process. The stored blob is
AEAD ciphertext with a key id so two wrap keys can be live during rotation.
That layout is not a public string and must not appear in env, vars, list
responses, or git.

Treat `K` like the other control-plane secrets: host-owned `.env`, injected
as an environment variable (not a flag, so it stays out of argv / `docker
inspect` command). Production compose already refuses to let CI rewrite
secrets; Actions only bumps `STRATEGON_VERSION`. Do not invent a
"GitHub-Actions-only" injection path.

Local / `auth-mode=none` / tests use an explicit dev key. Do not bake a
production-looking default into the binary.

Missing `K` does not prevent the control plane from starting. SecretManagement
goes dark: every Secret RPC fails, and resolve is unavailable. The rest of
the plane (machines, artifacts, plaintext assignments) keeps serving.

Operations:

- Every replica must share `K` (or the same key-id set).
- Rotation keeps the previous key id decrypt-only until every Secret row is
  re-sealed.
- Losing `K` means SecretManagement stays dark until `K` is restored. Back
  up `K` at the same sensitivity as the concatenation of every stored
  secret.
- If `K` leaks, rotate `K`, re-seal every Secret row, **and** rotate the
  underlying credentials. A database dump of the Secret store becomes
  plaintext once the attacker has `K`. Assignment rows and list responses
  only ever held `secret.<name>`, so they are not a historical ciphertext
  leak.

Do not decrypt in order to compare Secret rows. Re-`PutSecret` of the same
plaintext yields a new nonce; that is a store write, not an assignment
spec change. `SetAssignment` still uses `proto.Equal` on the spec, which
still contains `secret.<name>`.

---

## Human API

`PutSecret` on `ControlPlaneService`: name + plaintext in, `secret.<name>`
out. Upsert. Size-capped. Audited as "put secret <name>, N bytes, key id …"
with neither the input nor any ciphertext stored in `Detail`.

`GetSecret` / `ListSecrets`: name, length, key id. Never plaintext. Never
ciphertext.

There is no `Decrypt`. Any RPC that takes a Secret and returns plaintext
collapses the model.

Get / list / apply of assignment sets return the stored maps unchanged. If
the store has `secret.db-url`, the client sees `secret.db-url`.

`Deploy` (version-only) already clones env. Leave those values untouched.

---

## UI

The embedded SPA is a human surface. It obeys the same invariants as
`GetSecret` / `ListSecrets`: name, length, key id — never plaintext, never
ciphertext. `StrategyView` still has no env; machine and strategy pages do
not grow an env panel in order to "see" secrets.

**`/secrets` page** (sidebar: Deploy group, next to Artifacts — same class
of catalog as `/artifacts`, not Observe-next-to-tokens):

- List: name, public token `secret.<name>`, byte length, wrap key id.
  Empty state points at Put. No plaintext column, no "reveal", no download.
- Put form: name + value. Value is a password field. Submit calls
  `PutSecret`, then **clears the value field**. Show `secret.<name>` with a
  copy control. The name is listable afterwards; the plaintext is not.
- Overwrite is the same form (Put is upsert). Confirm that a running
  process keeps the old env until the next start.
- No delete button in v1. There is no `DeleteSecret` RPC; assignments would
  keep `secret.<name>` and the next resolve would fail closed.
- SecretManagement dark (missing `K`, every Secret RPC failing): the page
  stays reachable and shows that the module is down. Do not pretend the
  list is empty.

**Deploy (`/deploy`) env box:** still `KEY=value` lines. A value of
`secret.db-url` is valid. Offer a picker fed by `ListSecrets` (same pattern
as the artifact version select) and a link to `/secrets`. Do not fetch
plaintext to "help fill in".

**Sets / assignment reads:** if a screen ever prints env or vars, it prints
the stored token. Do not replace `secret.<name>` with bullets, a placeholder,
or a fetch-on-click unwrap.

**Audit:** `PutSecret` already lands in `ListAudit`. The audit page needs
no new viewer; `Detail` already forbids plaintext and ciphertext.

The browser must not persist the Put value (no `localStorage`, no replay
into the form). The SPA does not grow a Decrypt client.

---

## Write path and southbound

Two persisted copies of env hold the public token, not ciphertext:

1. `assignment_sets.spec` — template `env` and `member.vars`.
2. `assignments.spec` — per-member spec after `assignmentset.Expand`.

Order:

```
apply
  → expand placeholders (secret.<name> is inert)
  → reject unknown / broken secret. refs
  → persist secret.<name> on both rows
  → Notify
  → buildDesiredState clones rec.Assignments
  → live-resolve secret.<name> on the clone only
  → send DesiredState
```

Resolve never writes back. `computeAssignment` / `ApplyAssignment` must not
unwrap before `SetAssignment`.

`Expand` runs first so `${member.vars.*}` can resolve to `secret.<name>`;
the southbound clone then unwraps. Never resolve and then expand — a
password containing `${` would be scanned again.

`PutSecret` that changes plaintext does not rewrite assignment specs.
SecretManagement `Notify`s every machine that references the name.
`DesiredState.generation` may stay put; the agent already overwrites its
desired copy on every snapshot. Env is applied at process start only:
`versionMatches` compares artifact/config digest, so a running HEALTHY
process is not restarted. v1 is **next-start**, the same as shared files.
Rotating a password that must evict a running process is a later ticket
(it requires dropping "agent unchanged").

**Resolve failure does not publish a mixed snapshot.**

- SecretManagement dark (missing `K`) or any ref on that machine failing
  (deleted name, unknown key id, bad MAC): do not push a new `DesiredState`
  for that machine. The agent keeps the last snapshot it already has. Old
  env keeps running.
- `ApplyAssignment` that names a Secret it cannot use: that RPC fails.
- `ApplyAssignmentSet` that names a Secret it cannot use: the whole apply
  is rejected.

Never omit an assignment from `DesiredState` to "hold" it. The list is
full-snapshot; the agent `retire`s names that disappeared, which kills the
process. Never forward `secret.db-url` as the env value. The payload would
treat the token as `DATABASE_URL`.

The agent does not learn SecretManagement. It keeps receiving ordinary
`spec.Env`. No agent capability bump, no unwrap key on the machine.

`stopped` assignments still travel in `DesiredState` with their resolved
env. The agent holds those secrets even when the process is down. v1 does
not strip env on stop.

---

## Scope

| Surface | Secret refs? |
|---------|----------------|
| `StrategyAssignmentSpec.env` | yes |
| `SetMember.vars` (same `secret.<name>` grammar) | yes |
| `args` | no — not a Secret channel |
| Config artifacts, shared files, volumes | no |
| OCI image `Config.Env` | no |
| File browse / `DownloadFiles` | no — can still read on-disk config and `.stdio` |
| `capture_stdio` payload logs | no — the process can print its own env |
| SPA `/secrets`, Deploy env picker | yes — list/put/copy `secret.<name>` only |
| `GetMachine` / `WatchMachine` / strategy page | no — `StrategyView` has no env |

`supervision.json` already omits env. OCI `--oci-init` already keeps env
off argv. Those stay as they are.

If a credential must not appear in a list response, it goes in env or vars
as `secret.<name>`. It does not go in a config blob, a flag, or an image.

---

## Operator flow

1. The person who holds the plaintext calls `PutSecret("db-url", …)`
   (CLI/RPC or `/secrets`) and receives `secret.db-url`.
2. They give that name to the publisher. The publisher cannot see the
   secret and cannot cryptographically prove the stored value is the
   intended one; that is a process problem, not an API problem.
3. The publisher writes `DATABASE_URL: secret.db-url` in the manifest and
   applies.
4. Readers see `secret.db-url`. The assigned agent receives plaintext on
   the next process start.

Unsealed values remain valid so existing manifests do not break. v1 does
not require particular keys to be sealed.

---

## What this is not

- Not RBAC. SecretManagement hides plaintext from storage and read APIs;
  it does not create a viewer role.
- Not agent-blind injection. Something on the machine must `exec` with the
  env; here that is the agent.
- Not a substitute for rotating the database password after `K` leaks.
- Not offline GitOps sealing. Symmetric `PutSecret` needs a live control
  plane with `K`.
- Not encryption of config artifacts or file-browse results.
- Not an admission webhook that guesses which keys are credentials.
- Not a browser vault. The SPA never unwraps and never remembers the
  Put value.

---

## Invariants

1. No human RPC returns decrypted env, vars, or Secret payload.
2. No decrypt RPC exists.
3. Ciphertext never leaves the Secret store. Assignment tables store
   `secret.<name>`; only the `DesiredState` clone is resolved.
4. A resolve failure never delivers `secret.<name>` to the agent as a
   value, and never omits an assignment from the snapshot.
5. Missing `K` leaves the control plane up and SecretManagement dark.
6. `K` is host-owned, env-injected, keyed by id, and dual-live during
   rotation.
7. The public token is `secret.<name>` on every human surface, including
   the SPA.
8. The agent binary is unchanged. Secret updates are next-start.
9. The UI never displays, stores, or requests decrypted Secret payload.
