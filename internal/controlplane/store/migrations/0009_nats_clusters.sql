-- 0009_nats_clusters.sql — persist-only NatsCluster objects.
-- spec/status are protobuf BYTEA (same style as assignments). generation
-- bumps on spec change only; labels live inside metadata of the stored spec
-- snapshot but are also denormalized on the row for listing.

CREATE TABLE nats_clusters (
    name        TEXT PRIMARY KEY,
    uid         TEXT NOT NULL,
    generation  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    spec        BYTEA NOT NULL,
    status      BYTEA NOT NULL,
    labels      BYTEA
);
