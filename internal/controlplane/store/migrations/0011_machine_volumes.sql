-- 0011_machine_volumes.sql — machine-scoped named volumes (DesiredState.volumes).
--
-- volumes_generation bumps when the desired inventory changes; machines.generation
-- also bumps so the southbound DesiredState snapshot advances and agents reconverge.

ALTER TABLE machines
    ADD COLUMN volumes_generation BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN volumes_status BYTEA;

CREATE TABLE machine_volumes (
    machine_id TEXT   NOT NULL REFERENCES machines(machine_id) ON DELETE CASCADE,
    name       TEXT   NOT NULL,
    updated_at BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (machine_id, name)
);
