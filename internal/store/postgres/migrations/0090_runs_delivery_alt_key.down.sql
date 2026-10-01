-- 0090_runs_delivery_alt_key.down.sql — drop the second durable dedup key.
DROP INDEX IF EXISTS runs_delivery_alt_key;
ALTER TABLE runs DROP COLUMN IF EXISTS delivery_alt_key;
