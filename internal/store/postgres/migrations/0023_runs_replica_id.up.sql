-- v0.12.0 multi-replica HA: stamp every run with the replica_id that
-- owns its in-process state (cancel registry, semaphore slot, etc.).
--
-- Landed in Phase 1 as a nullable column with no writes — Phase 3 adds
-- the stamping at run creation, at which point cross-replica cancel
-- can look up the owner via this column instead of broadcasting blind.
-- Existing single-replica deployments see NULL on every row; nothing
-- reads it until Phase 3.

ALTER TABLE runs ADD COLUMN IF NOT EXISTS replica_id TEXT;
CREATE INDEX IF NOT EXISTS runs_by_replica ON runs(replica_id) WHERE replica_id IS NOT NULL;
