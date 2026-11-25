-- 0141_workspace_exposures.up.sql — RFC GW P9 PR 4: a workspace's exposed
-- ports (§5.3). One row per exposure; its id names the route
-- /v1/workspaces/exposures/{id}/…. Who may open it is read from the
-- workspace's own row on every request, so these rows decide nothing alone.
-- Deleted with their workspace (WorkspaceDelete, in one transaction), on
-- unexpose, and by the idle sweep (last_used_at). Mirrors the SQLite volumes
-- section.
CREATE TABLE IF NOT EXISTS workspace_exposures (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL DEFAULT '',
    user_id      TEXT NOT NULL DEFAULT '',
    workspace_id TEXT NOT NULL,
    port         INTEGER NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT 'page',
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ NOT NULL,
    UNIQUE (workspace_id, port)
);
CREATE INDEX IF NOT EXISTS workspace_exposures_by_workspace ON workspace_exposures (tenant_id, workspace_id);
CREATE INDEX IF NOT EXISTS workspace_exposures_by_last_used ON workspace_exposures (last_used_at);
