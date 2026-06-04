-- 0039_mcp_server_defs_tenant_id.down.sql — reverse RFC N's tenant axis
-- on the MCP server definition plane. Restores the single-column PK and
-- the UNIQUE(name, version) constraint, then drops the tenant_id columns.
--
-- NOTE: down is only safe while every row is tenant_id='' (the
-- single-tenant / pre-migration state). If real per-tenant rows exist,
-- restoring the single-col PK would collide on duplicate names — the
-- operator must reconcile names before rolling back.

ALTER TABLE mcp_server_def_active DROP CONSTRAINT mcp_server_def_active_pkey;
ALTER TABLE mcp_server_def_active ADD PRIMARY KEY (name);
ALTER TABLE mcp_server_def_active DROP COLUMN tenant_id;

ALTER TABLE mcp_server_defs DROP CONSTRAINT mcp_server_defs_tenant_name_version_key;
ALTER TABLE mcp_server_defs ADD CONSTRAINT mcp_server_defs_name_version_key UNIQUE (name, version);
ALTER TABLE mcp_server_defs DROP COLUMN tenant_id;
