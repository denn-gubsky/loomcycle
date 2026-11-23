-- 0136_workspace_shares.down.sql — drop RFC GW P8's shares: every workspace
-- is its owner's alone again.
DROP INDEX IF EXISTS workspaces_by_owner_agent;
ALTER TABLE workspaces DROP COLUMN IF EXISTS shares;
