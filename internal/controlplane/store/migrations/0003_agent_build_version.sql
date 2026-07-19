-- 0003_agent_build_version.sql — agent build string for ops display.
-- Distinct from agent_version (int32 capability). Display only.

ALTER TABLE machines
    ADD COLUMN IF NOT EXISTS agent_build_version TEXT NOT NULL DEFAULT '';
