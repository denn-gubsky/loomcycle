-- 0032_webhook_defs.up.sql — v1.x RFC H Input Webhooks storage.
--
-- A single content-addressed substrate Def with the same dual-table
-- shape as a2a_agent_defs / a2a_agent_def_active (0031) — minus the
-- sweeper-only run_state table, which was scheduler-specific.
--
-- webhook_defs declares an inbound HTTP webhook endpoint: the auth
-- scheme (hmac or bearer) + signing-secret env ref, rate limit,
-- delivery target (spawn an agent or publish to a channel), payload
-- mapping, and on_complete hooks.
--
-- definition is JSONB so future ops (find-similar-forks, etc.) can use
-- @> operators without a migration. Same shape A2AAgentDef uses. The
-- payload schema is owned by the tool layer (internal/tools/builtin);
-- the store treats it as opaque JSON.

CREATE TABLE webhook_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES webhook_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX webhook_defs_by_name   ON webhook_defs(name, version DESC);
CREATE INDEX webhook_defs_by_parent ON webhook_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX webhook_defs_by_run    ON webhook_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE webhook_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES webhook_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);
