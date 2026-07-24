-- 0034_memory_backend_defs.up.sql — RFC I MR-3a MemoryBackendDef storage.
--
-- A single content-addressed substrate Def with the same dual-table
-- shape as webhook_defs / webhook_def_active (0032) — a faithful
-- mirror, minus nothing (webhook_defs already dropped the
-- scheduler-specific run_state table).
--
-- memory_backend_defs declares a named memory backend: the kind
-- (the backend kind), connection config (base_url, api_version,
-- api_key_env), tenancy strategy (key_per_tenant or
-- shared_key_with_prefix), and fallback behaviour.
--
-- definition is JSONB so future ops (find-similar-forks, etc.) can use
-- @> operators without a migration. Same shape WebhookDef uses. The
-- payload schema is owned by the tool layer (internal/tools/builtin);
-- the store treats it as opaque JSON. Nothing consumes the Def yet —
-- the per-agent routing + backend factory land in MR-3b.

CREATE TABLE memory_backend_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES memory_backend_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX memory_backend_defs_by_name   ON memory_backend_defs(name, version DESC);
CREATE INDEX memory_backend_defs_by_parent ON memory_backend_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX memory_backend_defs_by_run    ON memory_backend_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE memory_backend_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES memory_backend_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);
