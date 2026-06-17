-- 0047_hooks_tenant_id.up.sql — RFC AF: tenant-scope the cluster-mode hook
-- registry so a `substrate:tenant` operator can register hooks confined to its
-- OWN tenant without holding `substrate:admin` (cross-tenant superuser).
--
-- Pre-RFC-AF the `hooks` table was global: a registration ignored the principal
-- and the in-memory Match() fired a hook on every run regardless of tenant.
-- This column carries the authoritative owning-tenant so a cluster reload /
-- backplane re-fetch reconstructs the scope, and the dispatcher's tenant filter
-- (hooks.Registry.Match) only fires a tenant-scoped hook on its own tenant's
-- runs.
--
-- '' = the operator/global/legacy hook (fires on EVERY run, preserving
-- pre-RFC-AF single-tenant + admin behaviour); existing rows backfill via the
-- column DEFAULT. The index mirrors hooks_by_owner (0026) for future
-- per-tenant admin queries; the hot-path Match() reads the in-memory cache, not
-- this index.

ALTER TABLE hooks ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS hooks_by_tenant ON hooks(tenant_id);
