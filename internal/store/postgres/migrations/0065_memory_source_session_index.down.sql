-- Reverses 0065. Dropping the index makes the provenance lookup a sequential scan
-- of the memory table; it does not change any answer, only how long it takes.
DROP INDEX IF EXISTS memory_by_source_session;
