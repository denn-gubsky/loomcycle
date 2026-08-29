DROP INDEX IF EXISTS memory_by_observed_at;
ALTER TABLE memory DROP COLUMN IF EXISTS observed_at;
