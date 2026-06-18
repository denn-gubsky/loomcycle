-- 0049_ephemeral_volume_defs.up.sql — RFC AH Phase 2b: ephemeral
-- (run-tree-scoped) volumes.
--
-- SEPARATE from volume_defs (Phase 2a). A Phase-2a dynamic volume is
-- tenant-shared with PK (tenant_id, name); an EPHEMERAL volume is scoped to
-- the creating run TREE — purged when the top-level run completes — so its
-- PK is (root_run_id, name). That difference is load-bearing: two concurrent
-- runs (even in ONE tenant) can each create a `work` volume with no clobber,
-- which a (tenant_id, name) PK could not express.
--
-- root_run_id is the TOP-LEVEL run id; the whole spawn tree shares it, so a
-- sub-agent's create lands under the same root and the tree resolves it via
-- the shared in-memory set. tenant_id is carried for the purge fence's
-- tenant-prefix check.
--
-- definition is the runtime-derived {"path":..,"mode":..} body. The path is
-- ALWAYS <dynamic_root>/_ephemeral/<root_run_id>/<name>, derived by the
-- VolumeDef tool from a caller-supplied NAME + MODE only — never a
-- caller-supplied host path. Both purge paths (inline run-completion + the
-- crash-recovery sweeper) RE-DERIVE the path rather than trust this stored
-- value, so a tampered row can't redirect an os.RemoveAll.

CREATE TABLE ephemeral_volume_defs (
    root_run_id TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    tenant_id   TEXT        NOT NULL DEFAULT '',
    definition  JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (root_run_id, name)
);
