-- 0101_runs_sessions_retention.up.sql — a run's retention mode.
--
-- '' is the default: everything a run says is stored, as before. 'none' is a
-- run whose content is delivered to whoever is waiting for it and stored
-- nowhere: its events are kept as markers (type, time, size), its result and
-- error as a class. The mode is decided when the run is created and cannot be
-- loosened afterwards.
--
-- It is a column and not something held in memory because the writers that
-- must honour it are not all in the process that started the run: another
-- replica appending a walk event, a hook decision that lands after the run
-- ended. Each has to be able to tell from the row.
--
-- On sessions too: a 'none' run always opens its own session, and what reads
-- a session without a run in hand (a continuation, a replay, the History
-- tool) refuses on this.
ALTER TABLE runs     ADD COLUMN IF NOT EXISTS retention TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS retention TEXT NOT NULL DEFAULT '';
