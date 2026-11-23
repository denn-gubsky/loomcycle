-- 0139_workspace_owner_kind.up.sql — RFC GW P9, D35: a workspace opened over
-- loomcycle's MCP server belongs to the session's token identity (its tenant
-- and subject), not to a run.
--
-- owner_kind is 'run' (every row before, and every row a run opens) or
-- 'principal'. A principal's row keeps its pin key in owner_run_id
-- ('principal:<hex sha256 of tenant NUL subject>'), which no run id can be,
-- so the run-keyed reads never match it. opened_token is the token
-- definition that opened it (its end releases the workspace); last_used_at
-- is when an op last reached it (the idle release). Both are read only for
-- principal rows. Mirrors the SQLite volumes section and column pass.
--
-- 0137 and 0138 are left to the RFC GW phases building beside this one.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS owner_kind TEXT NOT NULL DEFAULT 'run';
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS opened_token TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX IF NOT EXISTS workspaces_by_owner_kind ON workspaces (owner_kind, state);
