-- 0017_memory_embeddings.up.sql — v0.9.0 Vector Memory storage.
--
-- One row per (scope, scope_id, key); rows here mirror exactly one row
-- in `memory`. The FK CASCADE means a base-row delete drops the
-- embedding automatically; we never have orphan vectors.
--
-- The `embedding` column is pgvector's `vector` type without a fixed
-- dimension — rows under different embedder models can coexist (the
-- typical "I'm migrating from text-embedding-3-small to large; not
-- everything re-embedded yet" steady state). The `dimension` column
-- is the authoritative source for the row's vector dimension; the
-- Memory tool's search op compares against it BEFORE issuing the
-- cosine query so dimension mismatches surface as a typed
-- ErrDimensionMismatch instead of an opaque pgvector runtime error.
--
-- Indexing strategy: the v0.9.0 default is sequential scan with the
-- (scope, scope_id) partial filter, which is fine for typical memory
-- scopes (<10k rows). HNSW is intentionally NOT created here because
-- it requires a single-dimension column — operators with large scopes
-- AND a stable single-embedder steady state can opt in via:
--   CREATE INDEX ON memory_embeddings USING hnsw (embedding vector_cosine_ops);
-- (only valid after every row in the scope is at the same dimension).
-- Tracked as a v0.9.x perf follow-up.
--
-- Migration tolerance: this migration runs successfully on Postgres
-- WITHOUT pgvector installed. The CREATE EXTENSION call lives inside
-- a sub-block whose EXCEPTION clause swallows the failure with a
-- RAISE NOTICE; the table is only created when the extension loaded.
-- The CREATE TABLE uses EXECUTE so the planner doesn't resolve the
-- `vector` type at parse time (which would fail before the EXCEPTION
-- branch could run). Operators upgrading from v0.8.x without
-- pgvector get a clean migration; they can install pgvector and
-- re-run `migrate up` later to bring the table into existence. The
-- runtime SupportsVectors() check refuses vector ops when the table
-- is missing, so this is safe.

DO $migration$
DECLARE
    has_vector boolean := false;
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS vector;
        has_vector := true;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pgvector extension not available; memory_embeddings will not be created. Install pgvector (apt install postgresql-<ver>-pgvector or use the pgvector/pgvector docker image) and re-run `loomcycle migrate up` to enable v0.9.0 Vector Memory.';
    END;

    IF has_vector THEN
        EXECUTE $sql$
            CREATE TABLE IF NOT EXISTS memory_embeddings (
                scope       TEXT        NOT NULL,
                scope_id    TEXT        NOT NULL,
                key         TEXT        NOT NULL,
                provider    TEXT        NOT NULL,
                model       TEXT        NOT NULL,
                dimension   INTEGER     NOT NULL,
                embedding   vector      NOT NULL,
                embed_text  TEXT        NOT NULL,
                created_at  TIMESTAMPTZ NOT NULL,
                PRIMARY KEY (scope, scope_id, key),
                FOREIGN KEY (scope, scope_id, key) REFERENCES memory(scope, scope_id, key) ON DELETE CASCADE
            )
        $sql$;
        -- The search pre-filter: every search query starts with
        -- WHERE scope = $1 AND scope_id = $2; this index makes that
        -- O(1) regardless of overall table size.
        EXECUTE 'CREATE INDEX IF NOT EXISTS memory_embeddings_by_scope ON memory_embeddings(scope, scope_id)';
        -- Covers the v0.9.0 PR 4 admin endpoint
        -- `MemoryEmbedListByModel` ("which rows are NOT on my
        -- current embedder?"). Operators only hit this during
        -- embedder migrations.
        EXECUTE 'CREATE INDEX IF NOT EXISTS memory_embeddings_by_model ON memory_embeddings(scope, scope_id, provider, model)';
    END IF;
END
$migration$;
