-- 0041_a2a_agent_defs_tenant_id.up.sql — RFC N (completion):
-- tenant-scope the A2A remote-peer (A2AAgent) definition plane.
--
-- Second of the RFC-N-completion migrations (0040–0044). An A2AAgentDef
-- declares a remote A2A peer an agent can call as a tool; it is resolved
-- in a tool context under the calling agent's tenant, so without the
-- tenant axis one tenant's peers (and their credential refs) leak into
-- another's resolver. Mirror of 0040 / 0037, minus the dynamic_* tier
-- (A2AAgent has none).
--
-- '' = the shared/operator/legacy tenant; existing rows backfill via the
-- column DEFAULT, so single-tenant deployments are unchanged.

ALTER TABLE a2a_agent_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE a2a_agent_defs DROP CONSTRAINT a2a_agent_defs_name_version_key;
ALTER TABLE a2a_agent_defs ADD CONSTRAINT a2a_agent_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

ALTER TABLE a2a_agent_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE a2a_agent_def_active DROP CONSTRAINT a2a_agent_def_active_pkey;
ALTER TABLE a2a_agent_def_active ADD PRIMARY KEY (tenant_id, name);
