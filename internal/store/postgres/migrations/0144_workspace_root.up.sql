-- 0144_workspace_root.up.sql — RFC GW P13: a workspace on a user's folder.
--
-- root is the name of the folder a personal worker offers (RFC GW §4.5)
-- for a workspace that is that folder, never a copy; empty for every other
-- workspace, and so for every row before. A name only: the folder's path
-- never leaves the worker. Mirrors the SQLite volumes section and column
-- pass.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS root TEXT NOT NULL DEFAULT '';
