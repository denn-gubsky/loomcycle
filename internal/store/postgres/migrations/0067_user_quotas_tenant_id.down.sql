-- 0067_user_quotas_tenant_id.down.sql — reverse the user_quotas tenant axis.
--
-- SAFE ONLY while every row is tenant_id='' (a single-tenant cluster).
-- If real per-tenant rows exist, restoring the bare user_id PK would
-- collide across tenants; the operator must consolidate first. Mirrors
-- the 0066 down caveat. user_quotas is a transient counter, so the
-- worst case is a one-time reset of in-flight counts, not data loss.

ALTER TABLE user_quotas DROP CONSTRAINT IF EXISTS user_quotas_pkey;
ALTER TABLE user_quotas ADD PRIMARY KEY (user_id);
ALTER TABLE user_quotas DROP COLUMN IF EXISTS tenant_id;
