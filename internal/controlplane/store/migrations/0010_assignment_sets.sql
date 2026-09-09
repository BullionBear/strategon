-- 0010_assignment_sets.sql — persist-only AssignmentSet objects.
--
-- Supersedes 0009_nats_clusters.sql, which shipped on main before NatsCluster
-- was generalized. Migrations are tracked by filename, so a database that ran
-- 0009 still runs this one.
--
-- spec/status are protobuf BYTEA (same style as assignments). generation
-- bumps on spec change only; labels live inside metadata of the stored spec
-- snapshot but are also denormalized on the row for listing.

CREATE TABLE assignment_sets (
    name        TEXT PRIMARY KEY,
    uid         TEXT NOT NULL,
    generation  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    spec        BYTEA NOT NULL,
    status      BYTEA NOT NULL,
    labels      BYTEA
);

-- Drop the table 0009 created. Nothing reads it any more, and its rows hold
-- NatsCluster protobufs the new schema cannot decode, so there is nothing to
-- carry across. IF EXISTS keeps this a no-op on databases that never ran 0009.
-- The whole migration runs in one transaction, so a failure above leaves the
-- old table untouched.
DROP TABLE IF EXISTS nats_clusters;
