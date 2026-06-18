-- 0048_volume_defs.up.sql — RFC AH Phase 2a: the persistent dynamic
-- VolumeDef substrate.
--
-- Deliberately NOT the content-addressed / versioned Def shape the other
-- families use (agent_defs / skill_defs / mcp_server_defs / ...). A Volume
-- is a POINTER to mutable, stateful, on-disk content that lives OUTSIDE
-- the def — "fork a volume to v2" / "promote / roll back" are meaningless
-- (the files aren't in the def and aren't content-addressed). So this is a
-- FLAT (tenant_id, name) table: no version column, no parent_def_id, no
-- content_sha256, no active-pointer table.
--
-- definition is the runtime-derived {"path":..,"mode":..} body. The path is
-- ALWAYS <dynamic_root>/<tenant-segment>/<name>, derived by the VolumeDef
-- tool from a caller-supplied NAME + MODE only — never a caller-supplied
-- host path. The purge op re-derives the path rather than trusting this
-- stored value, so a tampered row can't redirect an os.RemoveAll.
--
-- '' = the shared/operator/legacy tenant (the RFC N axis). The PK is
-- (tenant_id, name) so two tenants own the same name independently with
-- no clobber.

CREATE TABLE volume_defs (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    name        TEXT        NOT NULL,
    definition  JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, name)
);
