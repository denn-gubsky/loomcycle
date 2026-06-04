-- 0037_agent_defs_tenant_id.down.sql — reverse RFC N's tenant axis on the
-- agent definition plane. Restores the single-column PKs and the
-- UNIQUE(name, version) constraint, then drops the tenant_id columns.
--
-- NOTE: down is only safe while every row is tenant_id='' (the
-- single-tenant / pre-migration state). If real per-tenant rows exist,
-- restoring the single-col PK would collide on duplicate names — the
-- operator must reconcile names before rolling back.

ALTER TABLE dynamic_agents DROP CONSTRAINT dynamic_agents_pkey;
ALTER TABLE dynamic_agents ADD PRIMARY KEY (name);
ALTER TABLE dynamic_agents DROP COLUMN tenant_id;

ALTER TABLE agent_def_active DROP CONSTRAINT agent_def_active_pkey;
ALTER TABLE agent_def_active ADD PRIMARY KEY (name);
ALTER TABLE agent_def_active DROP COLUMN tenant_id;

ALTER TABLE agent_defs DROP CONSTRAINT agent_defs_tenant_name_version_key;
ALTER TABLE agent_defs ADD CONSTRAINT agent_defs_name_version_key UNIQUE (name, version);
ALTER TABLE agent_defs DROP COLUMN tenant_id;
