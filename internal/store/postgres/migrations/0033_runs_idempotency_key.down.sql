DROP INDEX IF EXISTS runs_idempotency_key;
ALTER TABLE runs DROP COLUMN idempotency_key;
