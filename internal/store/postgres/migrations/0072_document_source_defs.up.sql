-- 0072_document_source_defs.up.sql — DocumentSourceDef storage.
--
-- A single content-addressed substrate Def with the same dual-table
-- shape as memory_backend_defs / memory_backend_def_active (0034 +
-- 0040) — a faithful structural mirror, tenant-isolated from the start.
--
-- document_source_defs declares a named external document source: the
-- kind, connection config (base_url, api_version, api_key_env), tenancy
-- strategy (key_per_tenant or shared_key_with_prefix), and fallback
-- behaviour.
--
-- definition is JSONB so future ops (find-similar-forks, etc.) can use
-- @> operators without a migration. The payload schema is owned by the
-- tool layer (internal/tools/builtin); the store treats it as opaque
-- JSON.
--
-- tenant_id is the RFC N tenant-isolation axis. '' = the shared/
-- operator/legacy tenant. The UNIQUE constraint is (tenant_id, name,
-- version), so two tenants own the same name independently; the active
-- pointer is per (tenant_id, name).

-- IF NOT EXISTS on every object: this migration lives in the
-- re-runnable window (> 61), so the migration idempotency tests
-- (TestMigrate0062_*) rewind the version pointer and re-apply it over
-- already-created tables. Every sibling migration >= 62 follows the same
-- convention.

CREATE TABLE IF NOT EXISTS document_source_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES document_source_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    tenant_id                 TEXT        NOT NULL DEFAULT '',
    UNIQUE(tenant_id, name, version)
);

CREATE INDEX IF NOT EXISTS document_source_defs_by_name   ON document_source_defs(name, version DESC);
CREATE INDEX IF NOT EXISTS document_source_defs_by_parent ON document_source_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS document_source_defs_by_run    ON document_source_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS document_source_def_active (
    name                  TEXT        NOT NULL,
    def_id                TEXT        NOT NULL REFERENCES document_source_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT,
    tenant_id             TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY(tenant_id, name)
);
