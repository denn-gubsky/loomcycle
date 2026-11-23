-- 0140_workspace_egress.down.sql — drop RFC GW P4's egress column: no
-- workspace has a way out.
ALTER TABLE workspaces DROP COLUMN IF EXISTS egress;
