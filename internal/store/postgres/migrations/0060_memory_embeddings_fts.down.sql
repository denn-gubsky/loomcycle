-- 0060_memory_embeddings_fts.down.sql — reverse the full-text leg: drop the
-- GIN index and the generated tsvector column. Guarded like the up so it's a
-- clean no-op on a Postgres without pgvector (no memory_embeddings table).
DO $migration$
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        DROP INDEX IF EXISTS memory_embeddings_fts;
        ALTER TABLE memory_embeddings DROP COLUMN IF EXISTS embed_text_tsv;
    END IF;
END
$migration$;
