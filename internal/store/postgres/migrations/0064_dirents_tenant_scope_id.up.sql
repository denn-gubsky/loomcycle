-- 0064_dirents_tenant_scope_id.up.sql — re-home tenant-scope dirents onto the
-- dirent plane's own scope-id convention.
--
-- The Document tool wrote a dirent using its SQL-Memory scope key. SQL Memory
-- requires a non-empty scope id (it becomes half of a schema name and a database
-- login role), so tenant scope carries the tenant there — while the dirent plane
-- leaves scope_id EMPTY for tenant and lets tenant_id carry the identity, which is
-- what the Path tool resolves. The result: a tenant document was named at a
-- coordinate nothing else reads, so it was invisible in the Path tree and the
-- browser, with no error on either side.
--
-- Data-only. The code fix keys new dirents correctly; this moves the rows written
-- before it. Rows created by the Path tool already use '' and are untouched by the
-- scope_id <> '' predicate.
--
-- A row whose destination coordinate is ALREADY occupied is left where it is
-- rather than merged: the '' row is the reachable one, so the stale row is a
-- duplicate that costs nothing, and choosing which resource_ref survives is not a
-- decision a migration should make silently. Idempotent — a second run finds only
-- those collisions and skips them again.
UPDATE dirents d
   SET scope_id = ''
 WHERE d.scope = 'tenant'
   AND d.scope_id <> ''
   AND NOT EXISTS (
         SELECT 1 FROM dirents x
          WHERE x.tenant_id = d.tenant_id
            AND x.scope = 'tenant'
            AND x.scope_id = ''
            AND x.parent_path = d.parent_path
            AND x.name = d.name);
