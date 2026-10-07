-- 0095_schedule_active_runs.up.sql — RFC DZ: finish each scheduled run once.
--
-- A scheduled run used to run INSIDE the sweeper's tick, under a 10-minute
-- cap, and its outcome was written when the call returned. Now the tick only
-- starts the run, and the run is finished when it ends — by the replica that
-- ran it, or, if that replica died, by a reconcile sweep on any replica.
--
-- schedule_active_runs holds one row per started, unfinished run. Whoever
-- DELETEs a run's row finishes it: records the outcome and dispatches its
-- on_complete hooks. A delete removes a row once, so hooks run exactly once
-- however the finishers race. No rows for a def = nothing running.
--
-- finished_at records when the schedule's last tracked run was finished.

CREATE TABLE IF NOT EXISTS schedule_active_runs (
    run_id     TEXT PRIMARY KEY,
    def_id     TEXT NOT NULL,
    slot_at    TIMESTAMPTZ NOT NULL,
    catch_up   BOOLEAN NOT NULL DEFAULT FALSE,
    started_at TIMESTAMPTZ NOT NULL,
    claimed_by TEXT
);

CREATE INDEX IF NOT EXISTS schedule_active_runs_def ON schedule_active_runs (def_id, started_at);

ALTER TABLE schedule_run_state ADD COLUMN IF NOT EXISTS finished_at TIMESTAMPTZ;
