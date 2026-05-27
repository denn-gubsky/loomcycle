-- v0.12.x — tag each process_samples row with the writing replica.
--
-- In cluster mode every replica writes its per-second CPU / RSS /
-- goroutine sample to the SAME shared process_samples table. Without
-- a replica tag the per-second series commingles across replicas and
-- can't be split for per-replica analysis (the multi-replica mock
-- stress suite needs exactly this split). The sampler stamps
-- LOOMCYCLE_REPLICA_ID here; single-replica deployments leave it NULL.
--
-- NULLable to preserve back-compat with pre-migration rows.
ALTER TABLE process_samples ADD COLUMN replica_id TEXT;

-- Partial index mirrors runs_by_replica: only cluster-mode rows carry
-- a replica_id, so index just those for the GROUP BY replica_id rollups.
CREATE INDEX process_samples_by_replica ON process_samples(replica_id) WHERE replica_id IS NOT NULL;
