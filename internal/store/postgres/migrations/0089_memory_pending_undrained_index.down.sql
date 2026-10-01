-- 0089_memory_pending_undrained_index.down.sql — drop the undrained-queue index.
DROP INDEX IF EXISTS memory_pending_undrained_by_scope;
