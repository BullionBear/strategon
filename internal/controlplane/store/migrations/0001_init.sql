-- 0001_init.sql — initial control-plane schema.
--
-- Rich protobuf messages (Register, StrategyAssignmentSpec, StatusReport
-- assignments, ArtifactRef, AuditEntry, MachineResources) are stored as
-- serialized bytes in BYTEA columns; the scalar columns the Store interface
-- reads or orders by (generation, heartbeat, reachable, ...) are promoted to
-- real columns. spec is written only by the control plane, status only by
-- agents (PROTOCOL.md §0).

CREATE TABLE machines (
    machine_id      TEXT PRIMARY KEY,
    register        BYTEA,
    reachable       BOOLEAN NOT NULL DEFAULT FALSE,
    agent_version   INTEGER NOT NULL DEFAULT 0,
    last_resources  BYTEA,
    last_heartbeat  BIGINT  NOT NULL DEFAULT 0,   -- unix seconds; 0 = never
    generation      BIGINT  NOT NULL DEFAULT 0,   -- monotonic desired-state version
    observed_gen    BIGINT  NOT NULL DEFAULT 0
);

-- Desired state: one row per assigned strategy. Removing a row (nil spec)
-- retires the strategy. generation is bumped on machines on every change.
CREATE TABLE assignments (
    machine_id  TEXT NOT NULL REFERENCES machines(machine_id) ON DELETE CASCADE,
    strategy    TEXT NOT NULL,
    spec        BYTEA NOT NULL,
    PRIMARY KEY (machine_id, strategy)
);

-- Observed state: latest per-strategy status reported by the agent.
CREATE TABLE statuses (
    machine_id  TEXT NOT NULL REFERENCES machines(machine_id) ON DELETE CASCADE,
    strategy    TEXT NOT NULL,
    status      BYTEA NOT NULL,
    PRIMARY KEY (machine_id, strategy)
);

-- Last artifact replaced by a Deploy, per strategy, for empty-target Rollback.
CREATE TABLE previous_artifacts (
    machine_id  TEXT NOT NULL REFERENCES machines(machine_id) ON DELETE CASCADE,
    strategy    TEXT NOT NULL,
    artifact    BYTEA NOT NULL,
    PRIMARY KEY (machine_id, strategy)
);

-- Content-addressed artifact catalog, keyed by name+version.
CREATE TABLE artifacts (
    name     TEXT NOT NULL,
    version  TEXT NOT NULL,
    ref      BYTEA NOT NULL,
    PRIMARY KEY (name, version)
);

-- Append-only audit log; id gives a stable newest-first ordering.
CREATE TABLE audit (
    id          BIGSERIAL PRIMARY KEY,
    machine_id  TEXT   NOT NULL DEFAULT '',
    strategy    TEXT   NOT NULL DEFAULT '',
    ts          BIGINT NOT NULL DEFAULT 0,   -- unix seconds
    entry       BYTEA  NOT NULL
);
CREATE INDEX audit_filter_idx ON audit (machine_id, strategy, id DESC);
