DROP INDEX IF EXISTS runs_pause_state_paused_idx;
ALTER TABLE runs DROP COLUMN IF EXISTS pause_state;
