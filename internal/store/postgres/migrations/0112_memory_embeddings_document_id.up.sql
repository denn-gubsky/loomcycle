-- 0112_memory_embeddings_document_id.up.sql — the document a Document row
-- belongs to, on its embedding.
--
-- A filtered document search restricts every retrieval leg to a corpus. Named
-- by chunk ids, a corpus of 50,000 chunks is a 50,000-element list bound into
-- each leg; named by document ids it is a few hundred. The column holds the
-- document of a chunk body (doc.chunk:<id>) and of its derived search units
-- (doc.unit:<id>:…); every other row leaves it NULL. NULL is also what a row
-- embedded before this migration carries until the document re-index pass
-- fills it, and a search falls back to chunk ids while any document row of
-- the scope still has none — so a missing value is slow, never wrong.
--
-- Two partial indexes:
--   * by_document serves `document_id = ANY(...)`: a narrow corpus is read
--     through the index instead of scanning the scope's partition.
--   * document_id_missing answers "does any document row of this scope still
--     lack one?" from the rows that lack one, which a fully filled scope keeps
--     to its non-document rows.
--
-- memory_embeddings exists only where pgvector loaded (0017 / 0062), so every
-- statement runs inside a to_regclass() check and is a clean no-op without it.
-- IF NOT EXISTS throughout, so a re-run is harmless.
DO $migration$
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        ALTER TABLE memory_embeddings ADD COLUMN IF NOT EXISTS document_id TEXT;

        CREATE INDEX IF NOT EXISTS memory_embeddings_by_document
            ON memory_embeddings (tenant_id, scope, scope_id, document_id)
            WHERE document_id IS NOT NULL;

        CREATE INDEX IF NOT EXISTS memory_embeddings_document_id_missing
            ON memory_embeddings (tenant_id, scope, scope_id, key)
            WHERE document_id IS NULL;
    END IF;
END
$migration$;
