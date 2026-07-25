-- 0062_memory_embeddings_repair.up.sql — create `memory_embeddings` on a
-- deployment that installed pgvector AFTER it had already migrated past 0017.
--
-- WHY THIS EXISTS (it looks redundant next to 0017 — it is not):
--
-- 0017 creates memory_embeddings only when `CREATE EXTENSION vector`
-- succeeded, and skips it with a RAISE NOTICE otherwise. That tolerance is
-- correct — a Postgres without pgvector must still migrate. But 0017's
-- comment used to tell operators they could "install pgvector and re-run
-- `loomcycle migrate up` later to bring the table into existence", and that
-- is FALSE: golang-migrate tracks a single monotonic version pointer and
-- only applies migrations strictly ABOVE it. A database sitting at version
-- 58 will never re-run 0017, so the table was unreachable FOREVER — the
-- operator had no supported path to enable Vector Memory short of hand-
-- writing the DDL. This migration is that path: a version ABOVE any shipped
-- pointer, so `migrate up` actually runs it.
--
-- It creates the table in its FULLY-MIGRATED post-0060 shape, because the
-- 0059 (tenant_id) and 0060 (embed_text_tsv) migrations are themselves
-- guarded on `to_regclass('memory_embeddings') IS NOT NULL` and were clean
-- no-ops on this deployment. Column order matters: 0017 creates the nine
-- base columns, 0059 APPENDS tenant_id, 0060 APPENDS embed_text_tsv, so a
-- from-scratch-with-pgvector database has ordinal_position 1..11 in exactly
-- the order below. Listing them in that order makes both paths converge on a
-- byte-identical table (asserted by
-- TestMigrate0062_RepairedTableMatchesFromScratchShape).
--
-- There is no partial-state case to handle: 0059/0060 either both ran (table
-- present, already complete → this migration no-ops) or both no-opped (table
-- absent → this migration creates it complete). The table can never exist in
-- a half-migrated shape.
--
-- Tolerance, mirroring 0017 exactly: the CREATE EXTENSION lives in a
-- sub-block whose EXCEPTION clause swallows the failure, and the DDL runs
-- via EXECUTE so the planner never resolves the `vector` type at parse time
-- (which would fail before the EXCEPTION branch could run). On a Postgres
-- without pgvector this migration applies successfully and does nothing.
--
-- ORDERING CAVEAT (be honest with operators): this migration runs ONCE, when
-- the version pointer crosses 62. Installing pgvector BEFORE upgrading to a
-- build carrying it is the clean path. An operator who installs pgvector
-- AFTER already being at version >= 62 is back in the same hole — the table
-- stays absent. That state is no longer a crash: Open() probes for the table
-- and degrades SupportsVectors()/SupportsFullText() to false (so memory ops
-- return the typed store.ErrVectorUnsupported), and logs one loud line
-- telling the operator what to do. There is deliberately NO boot-time
-- self-healing DDL path — loomcycle does not issue schema changes outside
-- the migration set.

DO $migration$
DECLARE
    has_vector boolean := false;
BEGIN
    BEGIN
        CREATE EXTENSION IF NOT EXISTS vector;
        has_vector := true;
    EXCEPTION WHEN OTHERS THEN
        RAISE NOTICE 'pgvector extension not available; memory_embeddings remains absent and Vector Memory stays disabled. Install pgvector (apt install postgresql-<ver>-pgvector or use the pgvector/pgvector docker image) BEFORE upgrading to this build so this migration can create the table.';
    END;

    -- to_regclass (not information_schema) so the check honours search_path:
    -- deployments and the test fixture both run under a non-default schema.
    IF has_vector AND to_regclass('memory_embeddings') IS NULL THEN
        RAISE NOTICE 'pgvector is present but memory_embeddings is missing (0017 ran in tolerant-skip mode before pgvector was installed); creating it now in its post-0060 shape.';
        EXECUTE $sql$
            CREATE TABLE memory_embeddings (
                scope          TEXT        NOT NULL,
                scope_id       TEXT        NOT NULL,
                key            TEXT        NOT NULL,
                provider       TEXT        NOT NULL,
                model          TEXT        NOT NULL,
                dimension      INTEGER     NOT NULL,
                embedding      vector      NOT NULL,
                embed_text     TEXT        NOT NULL,
                created_at     TIMESTAMPTZ NOT NULL,
                tenant_id      TEXT        NOT NULL DEFAULT '',
                embed_text_tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', embed_text)) STORED,
                PRIMARY KEY (tenant_id, scope, scope_id, key),
                FOREIGN KEY (tenant_id, scope, scope_id, key)
                    REFERENCES memory(tenant_id, scope, scope_id, key) ON DELETE CASCADE
            )
        $sql$;
        -- Same three indexes the 0017 → 0059 → 0060 path ends up with, same
        -- names and same leading-tenant_id column order.
        EXECUTE 'CREATE INDEX memory_embeddings_by_scope ON memory_embeddings(tenant_id, scope, scope_id)';
        EXECUTE 'CREATE INDEX memory_embeddings_by_model ON memory_embeddings(tenant_id, scope, scope_id, provider, model)';
        EXECUTE 'CREATE INDEX memory_embeddings_fts ON memory_embeddings USING GIN (embed_text_tsv)';
    END IF;
END
$migration$;
