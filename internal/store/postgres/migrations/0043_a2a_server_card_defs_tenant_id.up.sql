-- 0043_a2a_server_card_defs_tenant_id.up.sql — RFC N (completion):
-- tenant-scope the A2A server-card definition plane.
--
-- Fourth of the RFC-N-completion migrations (0040–0044). An
-- A2AServerCardDef declares which agents loomcycle exposes via A2A plus the
-- AgentCard metadata. The operator-configured server surface resolves one
-- card at boot under the operator tenant (""), but the AUTHORING plane was
-- global-by-name (create/fork/get/list/retire clobbered + leaked across
-- tenants). This adds the owning-tenant axis. Mirror of 0037, minus the
-- dynamic_* tier (A2A server card has none).
--
-- '' = the shared/operator/legacy tenant; existing rows backfill via the
-- column DEFAULT, so single-tenant deployments are unchanged.

ALTER TABLE a2a_server_card_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE a2a_server_card_defs DROP CONSTRAINT a2a_server_card_defs_name_version_key;
ALTER TABLE a2a_server_card_defs ADD CONSTRAINT a2a_server_card_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

ALTER TABLE a2a_server_card_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE a2a_server_card_def_active DROP CONSTRAINT a2a_server_card_def_active_pkey;
ALTER TABLE a2a_server_card_def_active ADD PRIMARY KEY (tenant_id, name);
