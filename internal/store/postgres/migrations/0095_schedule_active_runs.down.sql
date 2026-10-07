-- 0095_schedule_active_runs.down.sql — drop the RFC DZ run tracking.

ALTER TABLE schedule_run_state DROP COLUMN IF EXISTS finished_at;
DROP TABLE IF EXISTS schedule_active_runs;
