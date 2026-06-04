-- 0039_mcp_server_defs_tenant_id.up.sql — RFC N: tenant-scope the MCP
-- server definition plane (mirror of 0037/0038 for the MCP tables).
--
-- RFC L isolated the STATE plane (runs/sessions/memory keyed on the
-- authoritative principal). It left the DEFINITION plane global: the
-- mcp_server_defs / mcp_server_def_active tables had no tenant column, so
-- two tenants registering the same name collided on a single global active
-- pointer (last-writer-wins) and any token could resolve & dial another
-- tenant's MCP server. This migration adds the tenant axis, mirroring the
-- agent (0037) and skill (0038) definition planes.
--
-- '' = the shared/operator/legacy tenant. Existing rows backfill to ''
-- via the column DEFAULT, so a single-tenant deployment behaves exactly
-- as before. This PR completes the RFC N definition plane (agents +
-- skills + MCP servers).

-- mcp_server_defs: tenant the versioned-definition rows. The UNIQUE(name,
-- version) becomes UNIQUE(tenant_id, name, version) so two tenants can
-- own the same name independently.
ALTER TABLE mcp_server_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_server_defs DROP CONSTRAINT mcp_server_defs_name_version_key;
ALTER TABLE mcp_server_defs ADD CONSTRAINT mcp_server_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

-- mcp_server_def_active: the active pointer is now per (tenant_id, name).
-- Add the column, drop the single-col PK, add the composite PK.
ALTER TABLE mcp_server_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_server_def_active DROP CONSTRAINT mcp_server_def_active_pkey;
ALTER TABLE mcp_server_def_active ADD PRIMARY KEY (tenant_id, name);
