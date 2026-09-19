# Sealed env envelopes

Status: design — not implemented
Depends on: [Architecture](ARCHITECTURE.md)
Addresses: [issue #48](https://github.com/BullionBear/strategon/issues/48)

Assignment `env` (and `member.vars`) stay `map<string, string>`. Credentials
are stored as a **sealed envelope in the value**. The control plane holds the
unwrap key, human read APIs never return plaintext, and the only decrypt sink
is the southbound `DesiredState` clone pushed to the assigned agent.

This is not a Secret object, not `valueFrom`, and not an authorization model.
Auth stays flat: a session or API token is still a full operator.

---

## Why

Issue #48: env is a plain string map, persisted as protobuf, and
`ListAssignmentSets` / `GetAssignmentSet` echo it. A plane that needs
`DATABASE_URL` has nowhere safe to put it. The blast radius of a database
backup or a pasted list response is the production password.

Three things this design changes:

1. Values that have been sealed are ciphertext at rest and on every human RPC.
2. There is no decrypt / unwrap RPC.
3. The agent still receives ordinary plaintext env, because that is what
   `execve` requires. The agent is the last mile, not a new trust root.

It does not change who may apply, and it does not make unsealed values
illegal. Putting a password in env as ordinary text still works and is still
insecure. Credentials must use an envelope; that is an operator rule, not an
admission default in v1.

---

## Trust boundaries

| Place | After this design |
|-------|-------------------|
| `assignment_sets.spec` / `assignments.spec` | Envelope only |
| `GetAssignmentSet`, `ListAssignmentSets`, apply responses, CLI `get` | Envelope only |
| `GetMachine` / `WatchMachine` | Unchanged — `StrategyView` has no env |
| Audit `Detail` | Names, lengths, key id — never plaintext, never the envelope |
| Control plane process | Holds `K`; sees plaintext only while building the southbound clone |
| Agent + payload | Plaintext env (required for injection) |
| Host `/opt/strategon/deploy/.env` | `K`, same class as `SESSION_SECRET` / S3 keys |

A token that can only call read APIs sees ciphertext. A token that can also
`Apply*` can hang the same envelope on a machine the caller already controls;
the control plane will unwrap onto that agent. That is the intended decrypt
sink, not a read-API leak. Splitting "may list" from "may apply" is a
separate auth ticket.

---

## Envelope

The process name stays the real env key. The value is the envelope:

```text
DATABASE_URL=strsec1.<keyid>.<nonce>.<ct>
```

`strsec1` is the version tag. `<keyid>`, `<nonce>`, and `<ct>` are
unpadded base64url. That alphabet has no `$` or `{`, so control-plane
placeholder expansion cannot corrupt an envelope.

Do not encode secrecy in the key name (`STR_SECRET.DATABASE_URL=...`).
That forces a rename before `exec`, collides with template expansion, and
splits `DATABASE_URL` / `STR_SECRET.DATABASE_URL`.

Algorithm: AES-256-GCM (or equivalent AEAD). Fresh random nonce per
`Encrypt`. The envelope carries a key id so two wrap keys can be live
during rotation.

AAD is protocol context only (`env` / `v1`), **not** set name, machine, or
key name. Ciphertext is **portable**: encrypt once, hand the string to
whoever applies, paste it on any key or set. The unwrap key accepts it
anywhere. Binding to a resource would break that hand-off and is out of
scope.

Re-encrypting the same plaintext yields a new nonce and a different string.
`SetAssignment` compares specs with `proto.Equal`, so a re-seal looks like
an env change and rolls. Do not re-run `Encrypt` unless the secret itself
changed. Do not decrypt in order to compare — that pulls plaintext onto the
store hot path.

---

## Wrap key

The control plane owns a symmetric wrap key `K`. Encrypt is an RPC, so `K`
never leaves the process.

Treat `K` like the other control-plane secrets: host-owned `.env`, injected
as an environment variable (not a flag, so it stays out of argv / `docker
inspect` command). Production compose already refuses to let CI rewrite
secrets; Actions only bumps `STRATEGON_VERSION`. Do not invent a
"GitHub-Actions-only" injection path.

Local / `auth-mode=none` / tests use an explicit dev key. Do not bake a
production-looking default into the binary.

Operations:

- Every replica must share `K` (or the same key-id set).
- Rotation keeps the previous key id decrypt-only until every envelope is
  re-sealed.
- Losing `K` means no southbound publish of any sealed assignment. Back up
  `K` at the same sensitivity as the concatenation of every sealed secret.
- If `K` leaks, rotate `K`, re-seal every envelope, **and** rotate the
  underlying credentials. Historical envelopes in git, Postgres, and old
  list responses become plaintext once the attacker has `K`.

---

## Human API

`Encrypt` (name TBD on `ControlPlaneService`): plaintext in, envelope out.
Stateless. Size-capped. Audited as "encrypted N bytes, key id …" with
neither the input nor the output stored in `Detail`.

There is no `Decrypt`. Any RPC that takes an envelope and returns plaintext
collapses the model.

Get / list / apply return the stored maps unchanged. If the store has an
envelope, the client sees that envelope.

`Deploy` (version-only) already clones env. Leave those values untouched.

---

## Write path and southbound

Two persisted copies of env must stay sealed:

1. `assignment_sets.spec` — template `env` and `member.vars`.
2. `assignments.spec` — per-member spec after `assignmentset.Expand`.

Order:

```
apply
  → expand placeholders (envelope is inert)
  → persist envelope on both rows
  → Notify
  → buildDesiredState clones rec.Assignments
  → decrypt envelopes on the clone only
  → send DesiredState
```

Decrypt never writes back. `computeAssignment` / `ApplyAssignment` must not
unwrap before `SetAssignment`.

`Expand` runs first so `${member.vars.*}` can resolve to an envelope; the
southbound clone then unwraps. Never decrypt and then expand — a password
containing `${` would be scanned again.

Apply may reject a value that claims to be `strsec1` but is structurally
broken (wrong part count, bad base64, unknown version). It does not need
`K` and must not verify the MAC. MAC verification happens when building
the southbound clone.

**Decrypt failure does not publish.** Missing `K`, unknown key id, or bad
MAC: do not send `DesiredState` for that snapshot (or withhold that
assignment and do not start it). Never forward the envelope string as the
env value. The payload would treat ciphertext as `DATABASE_URL`.

The agent does not learn the envelope format. It keeps receiving ordinary
`spec.Env`. No agent capability bump, no unwrap key on the machine.

`stopped` assignments still travel in `DesiredState` with their env. The
agent holds those secrets even when the process is down. v1 does not strip
env on stop.

---

## Scope

| Surface | Sealed? |
|---------|---------|
| `StrategyAssignmentSpec.env` | yes |
| `SetMember.vars` (same envelope grammar) | yes |
| `args` | no — not a sealed channel |
| Config artifacts, shared files, volumes | no |
| OCI image `Config.Env` | no |
| File browse / `DownloadFiles` | no — can still read on-disk config and `.stdio` |
| `capture_stdio` payload logs | no — the process can print its own env |

`supervision.json` already omits env. OCI `--oci-init` already keeps env
off argv. Those stay as they are.

If a credential must not appear in a list response, it goes in env or vars
as an envelope. It does not go in a config blob, a flag, or an image.

---

## Operator flow

1. The person who holds the plaintext calls `Encrypt` and receives an
   envelope.
2. They send that string to the publisher over a channel they trust. The
   publisher cannot see the secret and cannot cryptographically prove the
   blob is the intended value; that is a process problem, not an API
   problem.
3. The publisher writes `DATABASE_URL: strsec1....` in the manifest and
   applies.
4. Readers see the same string. The assigned agent receives plaintext and
   starts the process.

Unsealed values remain valid so existing manifests do not break. v1 does
not require particular keys to be sealed.

---

## What this is not

- Not RBAC. Sealing hides plaintext from storage and read APIs; it does not
  create a viewer role.
- Not agent-blind injection. Something on the machine must `exec` with the
  env; here that is the agent.
- Not a substitute for rotating the database password after `K` leaks.
- Not offline GitOps sealing. Symmetric `Encrypt` needs a live control
  plane. The envelope layout (version + key id) is left extensible so a
  later public-key seal can produce the same `strsec1.…` shape.
- Not encryption of config artifacts or file-browse results.

---

## Invariants

1. No human RPC returns decrypted env or vars.
2. No decrypt RPC exists.
3. Both assignment tables store envelopes; only the `DesiredState` clone is
   unwrapped.
4. A decrypt failure never delivers the envelope to the agent as a value.
5. `K` is host-owned, env-injected, keyed by id, and dual-live during
   rotation.
6. Ciphertext is portable (no resource AAD).
7. The agent binary is unchanged.
