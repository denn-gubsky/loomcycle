-- 0148_workspace_exposure_state.down.sql — drop RFC HI P1's exposure state:
-- closed rows go (before 0148 a close deleted the row), then the old
-- one-page-per-port key comes back and the columns go.
DELETE FROM workspace_exposures WHERE closed_at IS NOT NULL;
DROP INDEX IF EXISTS workspace_exposures_by_user;
DROP INDEX IF EXISTS workspace_exposures_live_port;
ALTER TABLE workspace_exposures ADD CONSTRAINT workspace_exposures_workspace_id_port_key UNIQUE (workspace_id, port);
ALTER TABLE workspace_exposures DROP COLUMN IF EXISTS closed_reason;
ALTER TABLE workspace_exposures DROP COLUMN IF EXISTS closed_at;
ALTER TABLE workspace_exposures DROP COLUMN IF EXISTS lease_expires_at;
ALTER TABLE workspace_exposures DROP COLUMN IF EXISTS idle_timeout_s;
ALTER TABLE workspace_exposures DROP COLUMN IF EXISTS run_id;
ALTER TABLE workspace_exposures DROP COLUMN IF EXISTS workspace_name;
