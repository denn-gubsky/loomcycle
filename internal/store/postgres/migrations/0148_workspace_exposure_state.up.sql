-- 0148_workspace_exposure_state.up.sql — RFC HI P1: a page's lease, its own
-- idle timeout, and why it closed.
--
-- A close now marks the row (closed_at, closed_reason: unexposed, released,
-- idle or lost) instead of deleting it, and the exposure sweep purges closed
-- rows after a day (RFC HI D6), so a client can ask why its page went.
-- workspace_name and run_id are the workspace's name and owning run at
-- expose time: a closed row outlives its workspace's. idle_timeout_s is the
-- agent's own (NULL: the operator's); lease_expires_at is the client's lease
-- (RFC HI P2; NULL: none). Every row before reads as live, with no lease and
-- the operator's timeout.
--
-- The one-page-per-port key now holds among live rows only, or a closed
-- row would refuse its port to the next page. Mirrors the SQLite volumes
-- section, column pass and widened-key rebuild.
ALTER TABLE workspace_exposures ADD COLUMN IF NOT EXISTS workspace_name TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_exposures ADD COLUMN IF NOT EXISTS run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_exposures ADD COLUMN IF NOT EXISTS idle_timeout_s INTEGER;
ALTER TABLE workspace_exposures ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;
ALTER TABLE workspace_exposures ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;
ALTER TABLE workspace_exposures ADD COLUMN IF NOT EXISTS closed_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_exposures DROP CONSTRAINT IF EXISTS workspace_exposures_workspace_id_port_key;
CREATE UNIQUE INDEX IF NOT EXISTS workspace_exposures_live_port ON workspace_exposures (workspace_id, port) WHERE closed_at IS NULL;
CREATE INDEX IF NOT EXISTS workspace_exposures_by_user ON workspace_exposures (tenant_id, user_id);
