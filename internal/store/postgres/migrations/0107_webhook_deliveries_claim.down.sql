DROP INDEX IF EXISTS webhook_deliveries_by_claim_id;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS run_alt_key;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS run_key;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS claimed_at;
ALTER TABLE webhook_deliveries DROP COLUMN IF EXISTS claim_id;
