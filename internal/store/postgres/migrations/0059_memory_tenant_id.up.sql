-- 0059_memory_tenant_id.up.sql — RFC BL: tenant-scope the main Memory
-- store (the base k/v plane + its pgvector embeddings mirror).
--
-- RFC L isolated runs/sessions; RFC N isolated the definition plane. The
-- base `memory` table stayed tenant-blind: an agent scope_id is the bare
-- agent name, shared by same-named agents across tenants, so two tenants
-- writing the same (scope, scope_id, key) collided on one global row. This
-- migration adds the tenant axis (OQ #7) plus the RFC BL provenance/access
-- columns the hybrid ranker (PR2) will wire.
--
-- '' = the shared/operator/legacy tenant. Existing rows backfill to '' via
-- the column DEFAULT, so a single-tenant deployment reads exactly as before
-- (it IS entirely '') — no separate UPDATE needed.
--
-- FK lockstep (the sharp edge): memory's PRIMARY KEY cannot be dropped
-- while memory_embeddings' FK still references the old (scope, scope_id,
-- key). Order is therefore: add memory's columns → drop the embeddings FK →
-- swap memory's PK → widen + re-key the embeddings table → recreate the FK
-- on the 4-tuple. memory_embeddings lives behind a pgvector guard (0017
-- only creates it when the `vector` extension loaded), so every statement
-- that touches it runs inside a to_regclass() existence check and is a
-- clean no-op on a Postgres without pgvector.

-- memory: the tenant axis + RFC BL provenance/access columns. All additive;
-- the non-tenant columns are nullable/zero-defaulted so legacy rows read the
-- zero value.
ALTER TABLE memory ADD COLUMN tenant_id         TEXT        NOT NULL DEFAULT '';
ALTER TABLE memory ADD COLUMN origin            TEXT;
ALTER TABLE memory ADD COLUMN class             TEXT;
ALTER TABLE memory ADD COLUMN source_session_id TEXT;
ALTER TABLE memory ADD COLUMN source_run_id     TEXT;
ALTER TABLE memory ADD COLUMN access_count      BIGINT      NOT NULL DEFAULT 0;
ALTER TABLE memory ADD COLUMN last_accessed_at  TIMESTAMPTZ;

-- Drop the embeddings FK FIRST (guarded — the table only exists when
-- pgvector loaded) so memory's PK can be dropped below. The 0017 FK was
-- created inline (unnamed), so we resolve its actual name from the catalog
-- rather than hard-coding the auto-generated identifier.
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

-- memory: swap the PK to lead with tenant_id.
ALTER TABLE memory DROP CONSTRAINT memory_pkey;
ALTER TABLE memory ADD PRIMARY KEY (tenant_id, scope, scope_id, key);

-- memory_embeddings (guarded): add tenant_id, re-key, recreate the FK on the
-- 4-tuple (keeping ON DELETE CASCADE), and rebuild the scope/model indexes to
-- lead with tenant_id. tenant_id backfills to '' via the DEFAULT, matching
-- memory's backfill, so the FK holds on existing rows with no violation.
DO $migration$
BEGIN
    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        ALTER TABLE memory_embeddings ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';

        ALTER TABLE memory_embeddings DROP CONSTRAINT memory_embeddings_pkey;
        ALTER TABLE memory_embeddings ADD PRIMARY KEY (tenant_id, scope, scope_id, key);

        ALTER TABLE memory_embeddings
            ADD FOREIGN KEY (tenant_id, scope, scope_id, key)
            REFERENCES memory(tenant_id, scope, scope_id, key) ON DELETE CASCADE;

        DROP INDEX IF EXISTS memory_embeddings_by_scope;
        CREATE INDEX IF NOT EXISTS memory_embeddings_by_scope
            ON memory_embeddings(tenant_id, scope, scope_id);

        DROP INDEX IF EXISTS memory_embeddings_by_model;
        CREATE INDEX IF NOT EXISTS memory_embeddings_by_model
            ON memory_embeddings(tenant_id, scope, scope_id, provider, model);
    END IF;
END
$migration$;
