-- v0.8.15 LoomCycle MCP: dynamic_agents — runtime-registered agents
-- from `mcp__loomcycle__register_agent`. Survive restart until TTL
-- expiry (or explicit unregister). The `definition` column holds the
-- JSON-encoded config.AgentDef body verbatim (the store doesn't depend
-- on internal/config; same pattern as v0.8.5 agent_defs).
--
-- expires_at = epoch (1970-01-01 UTC) means "no expiry" (operator
-- must explicitly unregister). Non-default values are filtered at
-- read time so callers never see expired rows.

CREATE TABLE IF NOT EXISTS dynamic_agents (
    name        TEXT        PRIMARY KEY,
    definition  JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    description TEXT
);

-- Partial index: only TTL-bearing rows (the typical case). The full-table
-- scan for "no expiry" rows is fine since list operations are bounded
-- at 200 rows.
CREATE INDEX IF NOT EXISTS dynamic_agents_by_expires_at
    ON dynamic_agents(expires_at)
    WHERE expires_at > 'epoch';
