-- The OpenTelemetry trace a run's span was recorded in, so an operator can go
-- from a run to its trace (get_run, the run-start event, the Web UI link).
-- '' when tracing was off or the trace was unsampled: nothing was exported to
-- link to. Written once the span starts, after the row exists. Additive; no
-- backfill.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS trace_id TEXT NOT NULL DEFAULT '';
