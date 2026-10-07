-- 0094_schedule_run_state_claim.down.sql — drop the RFC DZ slot-claim columns.

ALTER TABLE schedule_run_state DROP COLUMN IF EXISTS claimed_at;
ALTER TABLE schedule_run_state DROP COLUMN IF EXISTS claimed_by;
ALTER TABLE schedule_run_state DROP COLUMN IF EXISTS slot_at;
