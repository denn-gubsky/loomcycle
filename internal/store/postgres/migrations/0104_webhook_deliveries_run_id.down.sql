DROP INDEX IF EXISTS webhook_deliveries_by_run_id;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS run_id;
