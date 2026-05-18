-- 0012_runs_pause_state.up.sql — v0.8.17 Pause/Resume primitive (Phase 1, PR 1).
--
-- Adds the pause_state column to runs. Three valid values are enforced at the
-- Store boundary (store.go's SetRunPauseState refuses anything else):
--   'running' — default; the run is executing or has terminated normally.
--   'pausing' — operator issued POST /v1/runtime/pause; the loop is winding
--               down to an iteration boundary.
--   'paused'  — loop reached the boundary and persisted; awaiting resume.
--
-- The partial index targets the resume path: ListPausedRuns scans for rows
-- where pause_state = 'paused', which is a tiny subset of the runs table
-- (most rows are 'running' or terminal). Partial index keeps the cost bound.
ALTER TABLE runs ADD COLUMN pause_state TEXT NOT NULL DEFAULT 'running';
CREATE INDEX runs_pause_state_paused_idx ON runs(started_at) WHERE pause_state = 'paused';
