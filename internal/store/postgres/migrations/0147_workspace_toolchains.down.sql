-- 0147_workspace_toolchains.down.sql — drop RFC HH's toolchains column:
-- every workspace's toolchains are unknown again.
ALTER TABLE workspaces DROP COLUMN IF EXISTS toolchains;
