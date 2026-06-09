-- 0042_schedule_defs_tenant_id.down.sql — reverse RFC N's tenant axis on
-- the Schedule definition plane. Only safe while every row is tenant_id=''
-- (restoring the single-col PK would collide on duplicate names otherwise).

ALTER TABLE schedule_def_active DROP CONSTRAINT schedule_def_active_pkey;
ALTER TABLE schedule_def_active ADD PRIMARY KEY (name);
ALTER TABLE schedule_def_active DROP COLUMN tenant_id;

ALTER TABLE schedule_defs DROP CONSTRAINT schedule_defs_tenant_name_version_key;
ALTER TABLE schedule_defs ADD CONSTRAINT schedule_defs_name_version_key UNIQUE (name, version);
ALTER TABLE schedule_defs DROP COLUMN tenant_id;
