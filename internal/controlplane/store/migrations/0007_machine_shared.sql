-- 0007_machine_shared.sql — machine-scoped shared files (DesiredState.shared).
--
-- Shared files are independent of assignment generations: shared_generation
-- bumps when the desired set changes; machines.generation also bumps so the
-- southbound DesiredState snapshot advances and agents reconverge.

ALTER TABLE machines
    ADD COLUMN shared_generation BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN shared_status BYTEA;

-- Desired shared files for a machine. artifact is the full ArtifactRef
-- (digest + uri) resolved at SetSharedFiles time for DesiredState materialization.
CREATE TABLE machine_shared_files (
    machine_id       TEXT   NOT NULL REFERENCES machines(machine_id) ON DELETE CASCADE,
    name             TEXT   NOT NULL,
    artifact_version TEXT   NOT NULL,
    digest           TEXT   NOT NULL,
    artifact         BYTEA  NOT NULL,
    updated_at       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (machine_id, name)
);
