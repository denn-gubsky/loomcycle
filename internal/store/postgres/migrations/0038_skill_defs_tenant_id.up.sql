-- 0038_skill_defs_tenant_id.up.sql — RFC N: tenant-scope the skill
-- definition plane (mirror of 0037 for the skill tables).
--
-- RFC L isolated the STATE plane (runs/sessions/memory keyed on the
-- authoritative principal). It left the DEFINITION plane global: the
-- skill_defs / skill_def_active tables had no tenant column, so two
-- tenants registering the same name collided on a single global active
-- pointer (last-writer-wins) and any token could resolve & run another
-- tenant's skill. This migration adds the tenant axis, mirroring the
-- agent definition plane (0037).
--
-- '' = the shared/operator/legacy tenant. Existing rows backfill to ''
-- via the column DEFAULT, so a single-tenant deployment behaves exactly
-- as before. This PR covers SKILLS only; MCP servers follow.

-- skill_defs: tenant the versioned-definition rows. The UNIQUE(name,
-- version) becomes UNIQUE(tenant_id, name, version) so two tenants can
-- own the same name independently.
ALTER TABLE skill_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_defs DROP CONSTRAINT skill_defs_name_version_key;
ALTER TABLE skill_defs ADD CONSTRAINT skill_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

-- skill_def_active: the active pointer is now per (tenant_id, name). Add
-- the column, drop the single-col PK, add the composite PK.
ALTER TABLE skill_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_def_active DROP CONSTRAINT skill_def_active_pkey;
ALTER TABLE skill_def_active ADD PRIMARY KEY (tenant_id, name);
