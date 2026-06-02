-- 0036_runs_tenant_id.down.sql — reverse the runs.tenant_id denormalization.
DROP INDEX IF EXISTS runs_by_tenant_active;
ALTER TABLE runs DROP COLUMN IF EXISTS tenant_id;
