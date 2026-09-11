# Epic: Machine-level volumes and OCI/EXEC mounts

Status: implemented — E1–E4
Depends on: [Architecture](ARCHITECTURE.md)
Closes: [issue #42](https://github.com/BullionBear/strategon/issues/42)

This document is the design record. It states the contract and the
reasoning behind it. Where the shipped code and this document disagree,
the code is right.

OCI writes outside `work/` and `shared/` land in `releases/<ver>/rootfs`
and vanish on `GCReleases`. The failure is silent: the process is
HEALTHY until the next tag. The same manifest must also work when the
launch artifact is EXEC, including auto-rollback from an OCI image to a
previous BINARY.

---

## Principles

1. **Volume is a machine object.** Identity is `(machine, name)`. Disk is
   `<base>/volumes/<name>`. Lifecycle is independent of assignment name,
   undeploy, and release GC.
2. **Assignment only mounts.** `volumeMounts: [{ name, containerPath }]`.
   No host path in the assignment. Missing name → do not start
   (`WaitingForVolume`). Mounts do not auto-create volumes.
3. **Southbound is desired inventory, not verbs.** Human
   `CreateVolume` / `DeleteVolume` write the store; the agent sees
   `DesiredState.volumes` and converges. `ControlMessage` already says
   DesiredState is truth.
4. **Delete is incremental and explicit.** There is no `SetVolumes`
   full-replace. The agent deletes a directory only when the name has
   left a **non-nil** desired list.
5. **v1 is local named directories.** No driver, NFS, quota, snapshot,
   or cross-machine attach.
6. **Mounts are driver-agnostic; binds are not.** `volumeMounts` are
   legal on EXEC and OCI. `containerPath` is always required and
   validated so one manifest applies to both. EXEC start ignores
   `containerPath` (no bind). OCI start bind-mounts
   `<base>/volumes/<name>` at `containerPath`. Path injection is
   `${VOLUME:<name>}` in args and (only that family) env. Resolution
   follows the **launch artifact**, not `spec.driver`, so auto-rollback
   from OCI to BINARY keeps a defined path.
7. **One live writer per volume.** A second running assignment mounting
   the same name fails start. Stopped assignments do not count as
   writers; they still pin `DeleteVolume`.
8. **UI follows Shared Files, not Artifacts.** No sidebar `/volumes`.
   Panel on the machine page; browse is a machine subpage; assignment
   page only shows mounts.

```
human write → store → Notify → DesiredState{volumes, assignments}
                            → agent mkdir / bind / expand
                            → StatusReport
```

---

## Object model

Inventory lives next to `MachineSharedSpec` on `DesiredState` (field 6).
The message has proto3 presence; **nil vs empty is load-bearing**.

| Snapshot | Agent |
|----------|--------|
| `DesiredState.volumes` unset (`nil`) | Do not create, do not delete, do not change volume status generation |
| non-nil, including empty list | Converge: mkdir desired, remove names that left the list |

`buildDesiredState` attaches `MachineVolumeSpec` only when
`volumes_generation > 0` or the map is non-empty. Deleting the last
volume still bumps generation, so the empty list is sent and GC runs.
A machine that has never used volumes keeps `nil`; stray directories
are left alone. An old control plane (no field 6) talking to a new
agent must not wipe `<base>/volumes/*`.

`VolumeMount.container_path` is **always required**, including on a
never-OCI assignment. That is a deliberate tradeoff: one manifest
validates the same way for EXEC and OCI, so auto-rollback and a later
image swap do not discover a missing path at start. EXEC ignores the
value at launch. The field is not optional in proto or apply.

Name rules match shared files: clean basename, no path separators, no
`.` / `..`, reserved `lost+found`. No user-supplied host path.

Container-path rules (apply-time and agent-time): absolute, cleaned,
not `/`; not a prefix of another mount on the same assignment; not
`/proc`, `/dev`, `/etc/resolv.conf`. Agent-only extra: must not equal
or be a prefix/parent of the three `bindSame` targets (work, shared,
config), which appear inside the container at their host paths.

---

## `${VOLUME:<name>}`

Agent `placeholderRE` stays `[A-Za-z_][A-Za-z0-9_]*` for
`${CONFIG}` / `${BINARY}` / `${RELEASE_DIR}`. Volume names include
`.` and `-` (`nats-a-data`), so a second match is required:

```
\$\{VOLUME:([^}/]+)\}
```

The capture is then passed through `volume.ValidateName`.

- **args:** all known placeholders, including `VOLUME`.
- **env:** only `${VOLUME:*}`; any other `${...}` stays verbatim
  (orchestration contract: `${CONFIG}` still does not expand in env).
- OCI → that mount's `containerPath`.
- EXEC → `filepath.Abs(VolumeDir(name))`.
- `name` must appear in this assignment's `volumeMounts`.

AssignmentSet expand treats `VOLUME:<basename>` as an agent
placeholder (pass through). `${set.*}` / `${member.*}` / `${peers}`
are expanded inside `volume_mounts[].name` and `container_path`.
Substitution is **innermost-first**: left-to-right `${([^}]*)}` on
`${VOLUME:${member.name}-data}` steals the first `}` and yields an
unknown body. After `${member.name}` → `nats-a`, the remainder
`${VOLUME:nats-a-data}` is left for the agent.

```yaml
volumeMounts:
  - { name: mftik-data, containerPath: /var/lib/mftik }
env:
  MFTIK_DATA: "${VOLUME:mftik-data}"
```

One-host set (avoids single-writer collision):

```yaml
template:
  volumeMounts:
    - { name: "${member.name}-data", containerPath: /var/lib/mftik }
  env:
    MFTIK_DATA: "${VOLUME:${member.name}-data}"
```

Renaming a member still recreates WorkDir / browse / status under
`<base>/<member.name>`. Durable process data survives only if the new
member remounts the same volume names. `${member.name}-data` isolates
writers on one host; it does not follow a rename.

---

## Human API

`CreateVolume` / `DeleteVolume` / `ListVolumes` — incremental, not
full-replace. Apply kind `MachineVolumes` is ensure-only (create
missing names, never delete). AssignmentSet apply does not create
volumes.

`DeleteVolume` occupancy is fail-closed:

1. Any desired assignment on that machine lists the mount (stopped
   still pins).
2. Any AssignmentSet whose **fully expanded** mount name for a member
   on that machine equals the volume — even if the controller has not
   yet written the assignment.

Scan with `assignmentset.Expand` per member. An expand error on a bad
set does not abort the RPC with Internal: keep scanning, treat that
set as occupied, reject the delete.

---

## Agent

Reconcile volumes before assignments. `awaitVolumeReady` gates
`startProcess` / `beginDeploy` the same way `awaitSharedReady` does.
`WaitingForVolume` is a WARNING event, not a new DeployPhase.

OCI `--oci-init` takes repeated `--oci-volume=/host:/container` and
bind-mounts host ≠ container path. EXEC ignores `VolumeBinds`.

After binds, cwd mkdir, `/.oldroot` removal, and a tmpfs on `/tmp`,
rootfs is remounted `MS_RDONLY` carrying `MS_NOSUID|MS_NODEV` (dropping
those locked flags is `EPERM` in a rootless userns). `/` is not remounted
`MS_NOEXEC` — the payload lives there and must exec. Writes to undeclared
paths become EROFS instead of silent data in `releases/`.

`size_bytes` is not a `du` on every heartbeat. Reports send the last
computed value or 0.

File browse of `<base>/volumes/<name>` uses the existing
`BrowseDir` / `DownloadFiles` RPCs with an optional `volume` field
(when set, `strategy` is ignored). Capability
`agent_version >= 3`.

---

## Ticket split

| Ticket | What landed |
|--------|-------------|
| E1 | Inventory proto, store, Create/Delete/List, DesiredState, agent mkdir/GC, CLI, machine panel, ensure-only apply |
| E2 | Volume browse (API + agent jail + `/machines/:id/volumes/:name`) |
| E3 | `volumeMounts`, `${VOLUME:name}`, AssignmentSet expand, OCI bind, start gate, single writer |
| E4 | `/tmp` tmpfs + rootfs remount RO + ARCHITECTURE / README persist contract |

---

## Non-goals

- Volume drivers, size quotas, snapshots, bind-from-host arbitrary paths
- Sharing a volume across machines
- Auto-create from `volumeMounts` or from AssignmentSet apply
- Overlay / persistent upperdir
- Expanding `${CONFIG}` / `${BINARY}` / `${RELEASE_DIR}` in env
- Migrating bytes already in `releases/<ver>/rootfs`
- A fleet `/volumes` catalog or assignment-page CRUD
