-- 0056_teamdefs.up.sql — TeamDef substrate.
--
-- Verbatim clone of the skill_defs substrate (0016 base + 0019
-- content_sha256 + 0038 tenant_id), collapsed into one migration
-- since teamdefs is a brand-new table with no upgrade path. The store
-- stays content-agnostic — the `definition` column carries an opaque
-- JSON-encoded workflow graph; the tool layer owns that JSON's schema.
--
-- Invariants mirrored from skill_defs:
--   * append-only (only INSERT + UPDATE of `retired`)
--   * (tenant_id, name, version) is UNIQUE and version is monotonic
--     per (tenant_id, name)
--   * parent_def_id chains lineage; empty for v1 / bootstrap rows
--   * content_sha256 NULL on hashless rows (partial index)
--   * tenant_id '' = the shared/operator/legacy tenant; two tenants
--     own the same name independently
--
-- Indexes:
--   teamdefs_by_name           — covers ListByName + GetByNameVersion
--   teamdefs_by_parent         — partial; covers ListChildren only when
--                                parent_def_id is set (most rows are v1)
--   teamdefs_by_run            — partial; covers provenance queries
--   teamdefs_by_content_sha256 — partial; covers the verify-by-hash path
CREATE TABLE teamdefs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES teamdefs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    content_sha256            TEXT,
    tenant_id                 TEXT        NOT NULL DEFAULT '',
    CONSTRAINT teamdefs_tenant_name_version_key UNIQUE (tenant_id, name, version)
);

CREATE INDEX teamdefs_by_name           ON teamdefs(name, version DESC);
CREATE INDEX teamdefs_by_parent         ON teamdefs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX teamdefs_by_run            ON teamdefs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;
CREATE INDEX teamdefs_by_content_sha256 ON teamdefs(content_sha256) WHERE content_sha256 IS NOT NULL;

-- The active pointer: at most one promoted version per (tenant_id, name).
-- Mirrors skill_def_active 1:1.
CREATE TABLE teamdef_active (
    tenant_id             TEXT        NOT NULL DEFAULT '',
    name                  TEXT        NOT NULL,
    def_id                TEXT        NOT NULL REFERENCES teamdefs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT,
    PRIMARY KEY (tenant_id, name)
);
