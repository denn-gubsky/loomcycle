-- F42 / RFC X Phase 2: persist whether a run is an interactive (persistent,
-- parks-at-end_turn) session. A snapshotted + restored paused run is
-- re-dispatched on the target instance by reconstructing its loop from the
-- transcript; the loop needs to know whether to park at end_turn
-- (interactive) or run to completion (batch). The flag was previously a
-- call-time-only RunOptions field, never persisted, so it couldn't survive
-- a snapshot. Additive nullable→default-false column; legacy + batch rows
-- read false. CreateRun stamps it at dispatch.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS interactive BOOLEAN NOT NULL DEFAULT FALSE;
