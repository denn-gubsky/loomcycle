-- 0097_runs_by_parent_run_id.up.sql — a run's children by its run id.
--
-- ListRunsByParentRunID finds the runs below one that is being ended with no
-- loop behind it. A team walk's members name the walk as their parent run but
-- the agent that started the walk as their parent agent, so they are only
-- found this way — and without an index that is a scan of runs per ended run.
-- Partial: a top-level run has no parent.
CREATE INDEX IF NOT EXISTS runs_by_parent_run_id ON runs (parent_run_id) WHERE parent_run_id IS NOT NULL;
