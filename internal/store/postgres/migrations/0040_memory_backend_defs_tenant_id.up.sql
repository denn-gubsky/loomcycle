-- 0040_memory_backend_defs_tenant_id.up.sql — RFC N (completion):
-- tenant-scope the MemoryBackend definition plane.
--
-- Migrations 0037–0039 added the tenant axis to agent / skill / MCP
-- server defs. This is the first of the RFC-N-completion migrations
-- (0040–0044) that finish the job across the remaining def families
-- (memory backend / A2A agent / schedule / A2A server card / webhook),
-- so EVERY content-addressed substrate Def is tenant-isolated and no
-- token can resolve, clobber, or promote another tenant's definition.
--
-- '' = the shared/operator/legacy tenant. Existing rows backfill to ''
-- via the column DEFAULT, so a single-tenant deployment behaves exactly
-- as before. Mirror of 0037, minus the dynamic_* tier (MemoryBackend has
-- none — it is static cfg + the memory_backend_defs/_active substrate).

-- memory_backend_defs: tenant the versioned-definition rows. The
-- UNIQUE(name, version) becomes UNIQUE(tenant_id, name, version) so two
-- tenants can own the same name independently.
ALTER TABLE memory_backend_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_backend_defs DROP CONSTRAINT memory_backend_defs_name_version_key;
ALTER TABLE memory_backend_defs ADD CONSTRAINT memory_backend_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

-- memory_backend_def_active: the active pointer is now per (tenant_id,
-- name). Add the column, drop the single-col PK, add the composite PK.
ALTER TABLE memory_backend_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE memory_backend_def_active DROP CONSTRAINT memory_backend_def_active_pkey;
ALTER TABLE memory_backend_def_active ADD PRIMARY KEY (tenant_id, name);
