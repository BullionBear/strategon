-- 0012_machine_slots.sql — agent-reported on-disk strategy slot inventory.
--
-- Written only by ApplyStatus when StatusReport.slots is non-nil (old agents
-- omit the wrapper; do not overwrite). Empty slots means the agent walked
-- and found nothing.

ALTER TABLE machines
    ADD COLUMN slots_status BYTEA;
