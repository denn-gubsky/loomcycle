-- 0133_tenant_telemetry_policies.down.sql — drop the per-tenant telemetry
-- destinations. Nothing references them.
DROP TABLE IF EXISTS tenant_telemetry_policies;
