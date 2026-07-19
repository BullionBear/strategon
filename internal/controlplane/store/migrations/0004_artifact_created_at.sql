-- 0004_artifact_created_at.sql — registration time for catalog "latest".
-- Also stored inside the ArtifactRef protobuf blob; the column enables ORDER BY
-- and backfills older rows that predate the proto field.

ALTER TABLE artifacts
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00';
