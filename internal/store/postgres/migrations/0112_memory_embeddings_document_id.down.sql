-- 0112_memory_embeddings_document_id.down.sql — drop the document id column
-- and its two indexes. Guarded like the up, so it is a clean no-op on a
-- Postgres without pgvector (no memory_embeddings table).
DO $migration$
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        DROP INDEX IF EXISTS memory_embeddings_document_id_missing;
        DROP INDEX IF EXISTS memory_embeddings_by_document;
        ALTER TABLE memory_embeddings DROP COLUMN IF EXISTS document_id;
    END IF;
END
$migration$;
