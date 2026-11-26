-- 0145_workspace_git.down.sql — drop RFC GW P11's git column: no workspace
-- is a git working tree.
ALTER TABLE workspaces DROP COLUMN IF EXISTS git;
