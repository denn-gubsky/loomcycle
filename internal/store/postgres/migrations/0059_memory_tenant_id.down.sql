-- 0059_memory_tenant_id.down.sql — reverse RFC BL's tenant axis + the
-- provenance/access columns on the Memory store. Restores the single-tenant
-- (scope, scope_id, key) PKs + the 3-tuple embeddings FK + the old indexes,
-- then drops the added columns.
--
-- NOTE: down is only safe while every row is tenant_id='' (the single-tenant
-- / pre-migration state). If real per-tenant rows exist, restoring the
-- 3-tuple PK would collide on duplicate (scope, scope_id, key) — the operator
-- must reconcile before rolling back. Same reversal order as the up in
-- reverse: drop the embeddings FK → revert memory's PK → revert the
-- embeddings table → drop memory's columns.

-- Drop the 4-tuple embeddings FK FIRST (guarded) so memory's PK can revert.
DO $migration$
DECLARE
    fk_name text;
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        SELECT conname INTO fk_name
        FROM pg_constraint
        WHERE conrelid = 'memory_embeddings'::regclass AND contype = 'f';
        IF fk_name IS NOT NULL THEN
            EXECUTE 'ALTER TABLE memory_embeddings DROP CONSTRAINT ' || quote_ident(fk_name);
        END IF;
    END IF;
END
$migration$;

-- memory: revert the PK to (scope, scope_id, key).
ALTER TABLE memory DROP CONSTRAINT memory_pkey;
ALTER TABLE memory ADD PRIMARY KEY (scope, scope_id, key);

-- memory_embeddings (guarded): revert the PK, drop tenant_id, recreate the
-- 3-tuple FK, and restore the original indexes.
DO $migration$
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        ALTER TABLE memory_embeddings DROP CONSTRAINT memory_embeddings_pkey;
        ALTER TABLE memory_embeddings ADD PRIMARY KEY (scope, scope_id, key);
        ALTER TABLE memory_embeddings DROP COLUMN tenant_id;

        ALTER TABLE memory_embeddings
            ADD FOREIGN KEY (scope, scope_id, key)
            REFERENCES memory(scope, scope_id, key) ON DELETE CASCADE;

        DROP INDEX IF EXISTS memory_embeddings_by_scope;
        CREATE INDEX IF NOT EXISTS memory_embeddings_by_scope
            ON memory_embeddings(scope, scope_id);

        DROP INDEX IF EXISTS memory_embeddings_by_model;
        CREATE INDEX IF NOT EXISTS memory_embeddings_by_model
            ON memory_embeddings(scope, scope_id, provider, model);
    END IF;
END
$migration$;

-- memory: drop the RFC BL columns.
ALTER TABLE memory DROP COLUMN last_accessed_at;
ALTER TABLE memory DROP COLUMN access_count;
ALTER TABLE memory DROP COLUMN source_run_id;
ALTER TABLE memory DROP COLUMN source_session_id;
ALTER TABLE memory DROP COLUMN class;
ALTER TABLE memory DROP COLUMN origin;
ALTER TABLE memory DROP COLUMN tenant_id;
