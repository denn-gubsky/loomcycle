-- 0061_memory_consolidation_substrate.down.sql — reverse the RFC BL P2 PR1
-- consolidation substrate: drop the two tables and the soft-archive column.
DROP TABLE IF EXISTS memory_cursors;
DROP TABLE IF EXISTS memory_pending;
ALTER TABLE memory DROP COLUMN IF EXISTS superseded_at;
