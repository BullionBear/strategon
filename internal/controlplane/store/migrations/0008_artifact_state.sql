-- 0008_artifact_state.sql — registration-time ingest lifecycle.
-- Expand-only: existing rows default READY (direct-register / pre-ingest catalog).

ALTER TABLE artifacts
    ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'READY',
    ADD COLUMN IF NOT EXISTS state_reason TEXT NOT NULL DEFAULT '';
