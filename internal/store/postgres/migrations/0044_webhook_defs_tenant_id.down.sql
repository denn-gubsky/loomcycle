-- 0044_webhook_defs_tenant_id.down.sql — reverse RFC N's tenant axis on
-- the Webhook definition plane. Only safe while every row is tenant_id=''
-- (restoring the single-col PK would collide on duplicate names otherwise).

ALTER TABLE webhook_def_active DROP CONSTRAINT webhook_def_active_pkey;
ALTER TABLE webhook_def_active ADD PRIMARY KEY (name);
ALTER TABLE webhook_def_active DROP COLUMN tenant_id;

ALTER TABLE webhook_defs DROP CONSTRAINT webhook_defs_tenant_name_version_key;
ALTER TABLE webhook_defs ADD CONSTRAINT webhook_defs_name_version_key UNIQUE (name, version);
ALTER TABLE webhook_defs DROP COLUMN tenant_id;
