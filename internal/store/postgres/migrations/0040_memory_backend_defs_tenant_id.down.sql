-- 0040_memory_backend_defs_tenant_id.down.sql — reverse RFC N's tenant
-- axis on the MemoryBackend definition plane. Restores the single-column
-- PK and the UNIQUE(name, version) constraint, then drops tenant_id.
--
-- NOTE: down is only safe while every row is tenant_id='' (the
-- single-tenant / pre-migration state). If real per-tenant rows exist,
-- restoring the single-col PK would collide on duplicate names — the
-- operator must reconcile names before rolling back.

ALTER TABLE memory_backend_def_active DROP CONSTRAINT memory_backend_def_active_pkey;
ALTER TABLE memory_backend_def_active ADD PRIMARY KEY (name);
ALTER TABLE memory_backend_def_active DROP COLUMN tenant_id;

ALTER TABLE memory_backend_defs DROP CONSTRAINT memory_backend_defs_tenant_name_version_key;
ALTER TABLE memory_backend_defs ADD CONSTRAINT memory_backend_defs_name_version_key UNIQUE (name, version);
ALTER TABLE memory_backend_defs DROP COLUMN tenant_id;
