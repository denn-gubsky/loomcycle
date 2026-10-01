-- 0089_memory_pending_undrained_index.up.sql — an index for the undrained-queue
-- lookups that name no tenant.
--
-- The operator-layer consolidation fan-out (MemoryPendingTargetsAllTenants) and
-- the snapshot read (SnapshotReadMemoryPending) filter `scope = ? AND drained_at
-- IS NULL`. The only index, memory_pending_by_target, leads with tenant_id, so it
-- cannot serve either, and every consolidation tick scanned the table's whole
-- history. Partial on `drained_at IS NULL`, so the index stays the size of the
-- live queue rather than of everything ever queued.
--
-- The drained rows themselves are now deleted by a periodic prune once they have
-- been drained longer than LOOMCYCLE_MEMORY_PENDING_DRAINED_TTL_MS (default 7
-- days); see MemoryPendingPruneDrained.
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- to 61 and replays every later migration, so a bare CREATE here would fail
-- `already exists` on that replay.
CREATE INDEX IF NOT EXISTS memory_pending_undrained_by_scope
    ON memory_pending(scope, created_at)
    WHERE drained_at IS NULL;
