-- 0013_secrets.sql — SecretManagement catalog.
--
-- Ciphertext + metadata only. Human APIs never read this table as plaintext.
-- Assignment specs store the public token secret.<name>, not these bytes.

CREATE TABLE secrets (
    name         TEXT PRIMARY KEY,
    ciphertext   BYTEA NOT NULL,
    key_id       TEXT NOT NULL,
    length_bytes INTEGER NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);
