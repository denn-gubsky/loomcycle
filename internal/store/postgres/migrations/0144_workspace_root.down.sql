-- 0144_workspace_root.down.sql — drop RFC GW P13's root column: no
-- workspace is on a user's folder.
ALTER TABLE workspaces DROP COLUMN IF EXISTS root;
