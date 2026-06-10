-- 0045_schedule_run_state_fire_count.down.sql — drop the RFC S / F36
-- lifetime fire-count column.

ALTER TABLE schedule_run_state DROP COLUMN fire_count;
