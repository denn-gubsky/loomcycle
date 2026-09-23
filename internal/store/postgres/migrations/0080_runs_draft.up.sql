-- RFC DI D5: configured (created, not started) runs.
--
-- A draft is a runs row in status 'configured' (runs.status has no CHECK
-- constraint, so the new value needs no change there). This column holds the
-- RAW request that will start it — segments, caller tool narrowing, metadata
-- and the per-run overrides as sent — minus secrets, which are supplied again
-- at start and never stored. Not run_config: that holds MERGED values and is
-- the run's spec; merging happens at start, against the definition as it is
-- then. Cleared when the run starts.
--
-- Opaque JSONB, like run_config and result. Additive and nullable: NULL on
-- every row that is not a draft.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS draft JSONB;

-- The expiry sweep reads drafts by age; partial, so it is as small as the set
-- of live drafts.
CREATE INDEX IF NOT EXISTS runs_configured_by_age ON runs (started_at) WHERE status = 'configured';
