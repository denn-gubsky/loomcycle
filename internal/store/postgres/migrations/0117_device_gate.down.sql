-- 0117_device_gate.down.sql — drop the RFC FA P5 device-login and per-tenant
-- gate policy tables. Nothing references them.
DROP TABLE IF EXISTS tenant_auth_policies;
DROP INDEX IF EXISTS device_authorizations_by_expiry;
DROP TABLE IF EXISTS device_authorizations;
