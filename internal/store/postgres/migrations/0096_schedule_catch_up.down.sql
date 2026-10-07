-- 0096_schedule_catch_up.down.sql — drop the RFC DZ catch-up columns.

ALTER TABLE schedule_run_state DROP COLUMN IF EXISTS missed_slots;
ALTER TABLE schedule_run_state DROP COLUMN IF EXISTS catch_up_until;
