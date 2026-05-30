-- 0031_a2a_defs.up.sql — v1.x RFC G A2A protocol integration storage.
--
-- Two content-addressed substrate Defs, each with the same dual-table
-- shape as schedule_defs / schedule_def_active (0029) — minus the
-- sweeper-only schedule_run_state table, which was scheduler-specific.
--
-- a2a_server_card_defs declares which loomcycle agents are exposed via
-- A2A + the AgentCard metadata (name, provider, capabilities,
-- security schemes). a2a_agent_defs declares a remote A2A peer that
-- loomcycle agents can call as a tool (agent_card_url or
-- endpoint+binding, auth scheme + credential_ref, expected_skills).
--
-- definition is JSONB so future ops (find-similar-forks, etc.) can use
-- @> operators without a migration. Same shape ScheduleDef uses. The
-- payload schema is owned by the tool layer (internal/tools/builtin);
-- the store treats it as opaque JSON.

CREATE TABLE a2a_server_card_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES a2a_server_card_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX a2a_server_card_defs_by_name   ON a2a_server_card_defs(name, version DESC);
CREATE INDEX a2a_server_card_defs_by_parent ON a2a_server_card_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX a2a_server_card_defs_by_run    ON a2a_server_card_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE a2a_server_card_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES a2a_server_card_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);

CREATE TABLE a2a_agent_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES a2a_agent_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX a2a_agent_defs_by_name   ON a2a_agent_defs(name, version DESC);
CREATE INDEX a2a_agent_defs_by_parent ON a2a_agent_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX a2a_agent_defs_by_run    ON a2a_agent_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE a2a_agent_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES a2a_agent_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);
