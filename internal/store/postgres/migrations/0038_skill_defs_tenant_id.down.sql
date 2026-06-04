-- 0038_skill_defs_tenant_id.down.sql — reverse RFC N's tenant axis on
-- the skill definition plane. Restores the single-column PK and the
-- UNIQUE(name, version) constraint, then drops the tenant_id columns.
--
-- NOTE: down is only safe while every row is tenant_id='' (the
-- single-tenant / pre-migration state). If real per-tenant rows exist,
-- restoring the single-col PK would collide on duplicate names — the
-- operator must reconcile names before rolling back.

ALTER TABLE skill_def_active DROP CONSTRAINT skill_def_active_pkey;
ALTER TABLE skill_def_active ADD PRIMARY KEY (name);
ALTER TABLE skill_def_active DROP COLUMN tenant_id;

ALTER TABLE skill_defs DROP CONSTRAINT skill_defs_tenant_name_version_key;
ALTER TABLE skill_defs ADD CONSTRAINT skill_defs_name_version_key UNIQUE (name, version);
ALTER TABLE skill_defs DROP COLUMN tenant_id;
