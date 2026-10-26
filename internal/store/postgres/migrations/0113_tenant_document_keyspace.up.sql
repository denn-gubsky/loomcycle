-- 0113_tenant_document_keyspace.up.sql — move tenant-scope document rows to the
-- Memory plane's tenant keyspace.
--
-- The Document tool keyed a tenant document's chunk bodies (doc.chunk:) and
-- derived units (doc.unit:) — and their embeddings — by its SQL Memory scope id.
-- SQL Memory cannot key on an empty component, so for tenant scope that id is the
-- tenant, or 'default' for the empty tenant. The Memory plane leaves scope_id
-- EMPTY for tenant and lets tenant_id carry the identity, and that is where a
-- tenant-scope Memory search, recall or get reads. The rows were invisible to it.
--
-- Data-only. The code fix keys new rows at ''; this moves the rows written before
-- it. The same statements live in sqlshared.MigrateTenantDocumentRows, which the
-- SQLite open path and the boot sweep run — the sweep also moves rows a replica
-- on the old binary writes during a rolling upgrade.
--
-- Only rows at exactly the old scope id move. A tenant document row at any other
-- non-empty scope id is a stray: left where it is and counted in the notice. A
-- key present at both the old scope id and '' is a conflict: the later write
-- (updated_at) is kept and the other deleted — never a failure.
--
-- memory_embeddings references memory by the full key with no ON UPDATE action,
-- so the two updates run as ONE statement and the foreign key is checked at its
-- end. The table exists only where pgvector loaded (to_regclass guard).
DO $migration$
DECLARE
    strays    bigint;
    conflicts bigint;
    moved     bigint;
BEGIN
    SELECT COUNT(*) INTO strays FROM memory m
     WHERE m.scope = 'tenant' AND m.scope_id <> ''
       AND (substr(m.key, 1, 10) = 'doc.chunk:' OR substr(m.key, 1, 9) = 'doc.unit:')
       AND m.scope_id <> CASE WHEN m.tenant_id = '' THEN 'default' ELSE m.tenant_id END;

    SELECT COUNT(*) INTO conflicts FROM memory o
      JOIN memory d ON d.tenant_id = o.tenant_id AND d.scope = 'tenant'
                   AND d.scope_id = '' AND d.key = o.key
     WHERE o.scope = 'tenant' AND o.scope_id <> ''
       AND (substr(o.key, 1, 10) = 'doc.chunk:' OR substr(o.key, 1, 9) = 'doc.unit:')
       AND o.scope_id = CASE WHEN o.tenant_id = '' THEN 'default' ELSE o.tenant_id END;

    -- Conflicts: a destination older than its old-keyspace row goes first (its
    -- embedding cascades), then every old row that still has a destination.
    DELETE FROM memory
     WHERE scope = 'tenant' AND scope_id = ''
       AND EXISTS (SELECT 1 FROM memory o
                    WHERE o.tenant_id = memory.tenant_id AND o.key = memory.key
                      AND o.scope = 'tenant' AND o.scope_id <> ''
                      AND (substr(o.key, 1, 10) = 'doc.chunk:' OR substr(o.key, 1, 9) = 'doc.unit:')
                      AND o.scope_id = CASE WHEN o.tenant_id = '' THEN 'default' ELSE o.tenant_id END
                      AND o.updated_at > memory.updated_at);
    DELETE FROM memory
     WHERE scope = 'tenant' AND scope_id <> ''
       AND (substr(key, 1, 10) = 'doc.chunk:' OR substr(key, 1, 9) = 'doc.unit:')
       AND scope_id = CASE WHEN tenant_id = '' THEN 'default' ELSE tenant_id END
       AND EXISTS (SELECT 1 FROM memory d
                    WHERE d.tenant_id = memory.tenant_id AND d.scope = 'tenant'
                      AND d.scope_id = '' AND d.key = memory.key);

    IF to_regclass('memory_embeddings') IS NOT NULL THEN
        EXECUTE $sql$
            WITH m AS (
                UPDATE memory SET scope_id = ''
                 WHERE scope = 'tenant' AND scope_id <> ''
                   AND (substr(key, 1, 10) = 'doc.chunk:' OR substr(key, 1, 9) = 'doc.unit:')
                   AND scope_id = CASE WHEN tenant_id = '' THEN 'default' ELSE tenant_id END
                RETURNING 1),
            e AS (
                UPDATE memory_embeddings SET scope_id = ''
                 WHERE scope = 'tenant' AND scope_id <> ''
                   AND (substr(key, 1, 10) = 'doc.chunk:' OR substr(key, 1, 9) = 'doc.unit:')
                   AND scope_id = CASE WHEN tenant_id = '' THEN 'default' ELSE tenant_id END
                RETURNING 1)
            SELECT COUNT(*) FROM m
        $sql$ INTO moved;
    ELSE
        UPDATE memory SET scope_id = ''
         WHERE scope = 'tenant' AND scope_id <> ''
           AND (substr(key, 1, 10) = 'doc.chunk:' OR substr(key, 1, 9) = 'doc.unit:')
           AND scope_id = CASE WHEN tenant_id = '' THEN 'default' ELSE tenant_id END;
        GET DIAGNOSTICS moved = ROW_COUNT;
    END IF;

    RAISE NOTICE 'tenant document keyspace: moved % rows, resolved % conflicts (later write kept), left % strays in place',
        moved, conflicts, strays;
END
$migration$;
