-- 0006_api_tokens.sql — durable API tokens for the human auth service.
--
-- Only the SHA-256 hash of the plaintext secret is stored. Soft-delete via
-- revoked_at keeps issuance/revocation history for audit. last_used is
-- best-effort telemetry (batched flush from the control plane).

CREATE TABLE api_tokens (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    username    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    last_used   TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);
