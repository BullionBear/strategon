-- 0002_leases.sql — durable fencing leases (one holder per strategy).
-- Survives control-plane restart so migration interlocking / mutual exclusion
-- is not lost when the process recycles (IMPROVEMENT B1 + B2 alignment).

CREATE TABLE leases (
    strategy    TEXT PRIMARY KEY,
    machine_id  TEXT        NOT NULL,
    lease_id    TEXT        NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    ttl_nanos   BIGINT      NOT NULL
);

CREATE INDEX leases_expires_at_idx ON leases (expires_at);
