-- 0082_hook_defs.up.sql — HookDef substrate.
--
-- A clone of teamdefs (0056 + 0077) minus bootstrapped_from_static and
-- operator_authored: a HookDef is only written from an operator surface, and
-- whether a hook may widen hosts is decided by the definition that names it,
-- so neither authority column has a meaning here. The store stays
-- content-agnostic — `definition` carries an opaque JSON hooks.Def (event,
-- match, body, fail_mode, timeout) whose schema the tool layer owns.
--
-- Invariants mirrored from teamdefs:
--   * append-only (only INSERT + UPDATE of `retired`, plus a whole-name
--     hard delete per tenant)
--   * (tenant_id, name, version) is UNIQUE and version is monotonic
--     per (tenant_id, name)
--   * tenant_id '' = the shared/operator tenant; two tenants own the same
--     name independently
--
-- IF NOT EXISTS throughout: the memory_embeddings repair test rewinds the
-- version pointer to 61 and replays every later migration, so a bare CREATE
-- here would fail `already exists` on that replay.
CREATE TABLE IF NOT EXISTS hook_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES hook_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    content_sha256            TEXT,
    tenant_id                 TEXT        NOT NULL DEFAULT '',
    CONSTRAINT hook_defs_tenant_name_version_key UNIQUE (tenant_id, name, version)
);

CREATE INDEX IF NOT EXISTS hook_defs_by_name           ON hook_defs(name, version DESC);
CREATE INDEX IF NOT EXISTS hook_defs_by_parent         ON hook_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS hook_defs_by_run            ON hook_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS hook_defs_by_content_sha256 ON hook_defs(content_sha256) WHERE content_sha256 IS NOT NULL;

-- The active pointer: at most one promoted version per (tenant_id, name).
CREATE TABLE IF NOT EXISTS hook_def_active (
    tenant_id             TEXT        NOT NULL DEFAULT '',
    name                  TEXT        NOT NULL,
    def_id                TEXT        NOT NULL REFERENCES hook_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT,
    PRIMARY KEY (tenant_id, name)
);
