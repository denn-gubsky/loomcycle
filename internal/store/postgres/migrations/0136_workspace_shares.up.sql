-- 0136_workspace_shares.up.sql — RFC GW P8: a workspace's shares.
--
-- shares is a JSON list of {run_id, agent_id, access, created_at}: the runs
-- of the same user the owner shared the workspace with (RFC GW §5.2). It is
-- on the row, so every replica checks a share where it routes an op, and a
-- share holds across a resume. Written by compare-and-set on the old text.
-- '[]' for every row before: no workspace was shared.
--
-- (tenant_id, owner_agent_id, name) is how a peer names a shared workspace,
-- `<name>@<agent_id>`. Mirrors the SQLite volumes section and column pass.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS shares TEXT NOT NULL DEFAULT '[]';
CREATE INDEX IF NOT EXISTS workspaces_by_owner_agent ON workspaces (tenant_id, owner_agent_id, name);
