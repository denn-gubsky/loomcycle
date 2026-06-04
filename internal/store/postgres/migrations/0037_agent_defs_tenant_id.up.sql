-- 0037_agent_defs_tenant_id.up.sql — RFC N: tenant-scope the agent
-- definition plane.
--
-- RFC L isolated the STATE plane (runs/sessions/memory keyed on the
-- authoritative principal). It left the DEFINITION plane global: the
-- agent_defs / agent_def_active / dynamic_agents tables had no tenant
-- column, so two tenants registering the same name collided on a single
-- global active pointer (last-writer-wins) and any token could resolve &
-- run another tenant's agent. This migration adds the tenant axis the
-- 0006_agent_defs comment anticipated ("Per-tenant active pointers
-- (future v0.9.x) extend the PK to (tenant_id, name)").
--
-- '' = the shared/operator/legacy tenant. Existing rows backfill to ''
-- via the column DEFAULT, so a single-tenant deployment behaves exactly
-- as before. This PR covers AGENTS only; skills + MCP servers follow.

-- agent_defs: tenant the versioned-definition rows. The UNIQUE(name,
-- version) becomes UNIQUE(tenant_id, name, version) so two tenants can
-- own the same name independently.
ALTER TABLE agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_defs DROP CONSTRAINT agent_defs_name_version_key;
ALTER TABLE agent_defs ADD CONSTRAINT agent_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

-- agent_def_active: the active pointer is now per (tenant_id, name). Add
-- the column, drop the single-col PK, add the composite PK.
ALTER TABLE agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_def_active DROP CONSTRAINT agent_def_active_pkey;
ALTER TABLE agent_def_active ADD PRIMARY KEY (tenant_id, name);

-- dynamic_agents: same shape — per (tenant_id, name).
ALTER TABLE dynamic_agents ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE dynamic_agents DROP CONSTRAINT dynamic_agents_pkey;
ALTER TABLE dynamic_agents ADD PRIMARY KEY (tenant_id, name);
