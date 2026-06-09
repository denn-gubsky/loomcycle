-- 0043_a2a_server_card_defs_tenant_id.down.sql — reverse RFC N's tenant
-- axis on the A2A server-card definition plane. Only safe while every row
-- is tenant_id='' (restoring the single-col PK would collide on duplicate
-- names otherwise).

ALTER TABLE a2a_server_card_def_active DROP CONSTRAINT a2a_server_card_def_active_pkey;
ALTER TABLE a2a_server_card_def_active ADD PRIMARY KEY (name);
ALTER TABLE a2a_server_card_def_active DROP COLUMN tenant_id;

ALTER TABLE a2a_server_card_defs DROP CONSTRAINT a2a_server_card_defs_tenant_name_version_key;
ALTER TABLE a2a_server_card_defs ADD CONSTRAINT a2a_server_card_defs_name_version_key UNIQUE (name, version);
ALTER TABLE a2a_server_card_defs DROP COLUMN tenant_id;
