-- 0065_memory_source_session_index.up.sql — make provenance searchable.
--
-- source_session_id has been written on every consolidated fact since the memory
-- tier's first phase, and read back one row at a time, but never searched. The
-- decision it exists to serve — "provenance is mandatory for user scope so that
-- erasure is enumerable" — needs the other direction: given a subject's chats,
-- which facts anywhere derive from them.
--
-- That question cannot be answered from a subject-keyed delete, because a fact
-- ABOUT someone written into an agent or tenant scope is not keyed to them. Their
-- chats are, which makes this index the join that reaches the rest.
--
-- PARTIAL, because the column is NULL on every row that was not distilled from a
-- chat — which is most of them. Indexing those would pay for the majority to
-- support a query that never asks about them.
CREATE INDEX IF NOT EXISTS memory_by_source_session
    ON memory (tenant_id, source_session_id)
    WHERE source_session_id IS NOT NULL;
