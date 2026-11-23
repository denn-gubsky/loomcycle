-- 0139_workspace_owner_kind.down.sql — drop RFC GW P9's principal owners.
-- Remove principal-owned rows first: without owner_kind they would read as
-- runs whose row is gone, and be swept.
DELETE FROM workspaces WHERE owner_kind = 'principal';
DROP INDEX IF EXISTS workspaces_by_owner_kind;
ALTER TABLE workspaces DROP COLUMN IF EXISTS last_used_at;
ALTER TABLE workspaces DROP COLUMN IF EXISTS opened_token;
ALTER TABLE workspaces DROP COLUMN IF EXISTS owner_kind;
