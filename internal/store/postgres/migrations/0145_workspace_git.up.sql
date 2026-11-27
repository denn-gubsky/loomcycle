-- 0145_workspace_git.up.sql — RFC GW P11: a git workspace's repository.
--
-- git is a JSON object for a git workspace (its repository URL, its own
-- branch, the ref and commit it was opened from, the forge's default branch
-- as the core read it, the credential's reference and the worker's mirror
-- key), and empty for every other workspace, so for every row before. It
-- names a credential, never holds one. Mirrors the SQLite volumes section and
-- column pass.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS git TEXT NOT NULL DEFAULT '';
