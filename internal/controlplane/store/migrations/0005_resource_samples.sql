-- 0005_resource_samples.sql — short-term sliding window for resource trends.
--
-- Fixed retention (~1 hour); not a TSDB. Machine-level rows use strategy=''.
-- last_processes holds the latest Heartbeat process snapshot (BYTEA proto).

ALTER TABLE machines ADD COLUMN IF NOT EXISTS last_processes BYTEA;

CREATE TABLE IF NOT EXISTS resource_samples (
    machine_id  TEXT NOT NULL REFERENCES machines(machine_id) ON DELETE CASCADE,
    strategy    TEXT NOT NULL DEFAULT '',
    sampled_at  TIMESTAMPTZ NOT NULL,
    cpu_percent DOUBLE PRECISION,
    mem_bytes   BIGINT,
    PRIMARY KEY (machine_id, strategy, sampled_at)
);

CREATE INDEX IF NOT EXISTS resource_samples_lookup_idx
    ON resource_samples (machine_id, strategy, sampled_at DESC);
