-- 0047_hooks_tenant_id.down.sql — reverse RFC AF's tenant axis on the hook
-- registry. Safe unconditionally: tenant_id is an additive column with no PK /
-- unique-constraint dependency (the hooks table keys on id), so dropping it
-- cannot collide.

DROP INDEX IF EXISTS hooks_by_tenant;

ALTER TABLE hooks DROP COLUMN tenant_id;
