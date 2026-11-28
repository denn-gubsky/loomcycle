-- 0147_workspace_toolchains.up.sql — RFC HH §5 (D4): the toolchains a
-- workspace was opened with.
--
-- toolchains is a JSON list of names, what a template saved from the
-- workspace requires. NULL is unknown, as for every row before: save_template
-- then falls back to the source worker's list. '[]' is a workspace that
-- required none. Mirrors the SQLite volumes section and column pass.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS toolchains TEXT;
