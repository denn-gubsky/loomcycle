-- 0052_token_usage_and_run_cost.down.sql — reverse RFC AV Phase 1 schema.
DROP TABLE IF EXISTS token_usage;
ALTER TABLE runs DROP COLUMN IF EXISTS cost;
ALTER TABLE runs DROP COLUMN IF EXISTS cost_currency;
ALTER TABLE runs DROP COLUMN IF EXISTS credential_source;
ALTER TABLE runs DROP COLUMN IF EXISTS credential_scope_id;
