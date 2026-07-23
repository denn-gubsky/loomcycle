-- 0060_memory_embeddings_fts.up.sql — RFC BL P1 PR2: the full-text
-- (keyword) retrieval leg of hybrid memory search.
--
-- memory_embeddings.embed_text is the canonical searchable text — the same
-- text the vector leg embedded — so a tsvector over it covers exactly the
-- embedded-entry population MemoryEmbedSearch ranks. We add a STORED
-- generated column (kept in lockstep with embed_text automatically, no
-- application write path) plus a GIN index; the in-process backend then runs
-- vector ∥ full-text and fuses the two orderings via Reciprocal Rank Fusion.
--
-- memory_embeddings lives behind the pgvector guard (0017 creates it only
-- when the `vector` extension loaded), so every statement runs inside a
-- to_regclass() existence check and is a clean no-op on a Postgres without
-- pgvector. embed_text is TEXT NOT NULL (0017), so to_tsvector has no NULL
-- to guard against.
DO $migration$
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        ALTER TABLE memory_embeddings
            ADD COLUMN IF NOT EXISTS embed_text_tsv tsvector
            GENERATED ALWAYS AS (to_tsvector('english', embed_text)) STORED;

        CREATE INDEX IF NOT EXISTS memory_embeddings_fts
            ON memory_embeddings USING GIN (embed_text_tsv);
    END IF;
END
$migration$;
