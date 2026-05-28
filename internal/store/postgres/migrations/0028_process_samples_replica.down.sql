DROP INDEX IF EXISTS process_samples_by_replica;
ALTER TABLE process_samples DROP COLUMN replica_id;
