-- 0140_workspace_egress.up.sql — RFC GW P4: the routes a workspace may use.
--
-- egress is a JSON list of route names, recorded when the workspace is
-- opened: the routes it may send requests out through (RFC GW §6.3). The
-- core checks a worker's request against this row, never against what the
-- worker says. '[]' for every row before: no workspace had a way out.
-- Mirrors the SQLite volumes section and column pass.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS egress TEXT NOT NULL DEFAULT '[]';
