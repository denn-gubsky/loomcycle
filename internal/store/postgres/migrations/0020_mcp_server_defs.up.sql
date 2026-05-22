-- 0020_mcp_server_defs.up.sql — v0.9.x dynamic MCP server registration.
--
-- Extends the AgentDef (v0.8.5) + SkillDef (v0.8.22) substrate pattern
-- to MCP server registrations. Operators (or external orchestrators
-- via the bearer-authed admin endpoint) can register new MCP servers
-- at runtime without restarting loomcycle — e.g. n8n publishes a
-- workflow with MCP Server Trigger enabled and immediately makes its
-- tools available to loomcycle agents.
--
-- Coexists with the static yaml `mcp_servers:` block: yaml entries
-- stay boot-loaded as before; dynamic registrations live alongside.
-- The MCPServerDef tool refuses `create` on a name colliding with a
-- yaml entry (mirror of AgentDef.create refusal over cfg.Agents).
--
-- Stdio transport is yaml-only — dynamic registration accepts only
-- `http` and `streamable-http`. Closes the agent-spawned-subprocess
-- escalation path.
--
-- definition JSONB carries the operator-authored content (transport,
-- url, headers map, plus the cached discovered_tools list refreshed
-- via the tool's `rediscover` op). content_sha256 is the v0.9.x
-- deterministic hash of the content-bearing subset (excludes
-- discovered_tools because that's a cache, not authored content) —
-- see internal/mcp/Sign for the algorithm.

CREATE TABLE mcp_server_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES mcp_server_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    content_sha256            TEXT,
    UNIQUE (name, version)
);

CREATE INDEX mcp_server_defs_by_name   ON mcp_server_defs(name, version DESC);
CREATE INDEX mcp_server_defs_by_parent ON mcp_server_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX mcp_server_defs_by_run    ON mcp_server_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;
CREATE INDEX mcp_server_defs_by_content_sha256
    ON mcp_server_defs(content_sha256)
    WHERE content_sha256 IS NOT NULL;

-- Active-pointer overlay. Same shape as agent_def_active / skill_def_active.
-- One row per name; promoting a new version replaces the def_id.
CREATE TABLE mcp_server_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES mcp_server_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);
