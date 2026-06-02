-- 0036_runs_tenant_id.up.sql — denormalize the authoritative tenant onto
-- the runs table (RFC L / Web-UI multi-tenant authorization).
--
-- runs previously carried no tenant_id (only sessions did), so a
-- tenant-scoped workspace list needed a sessions JOIN on every read. The
-- Web UI's per-tenant view polls these lists, so we denormalize the
-- tenant onto the run row and index it. Set at run creation from the
-- run's effective tenant; the backfill copies it from the parent session
-- for pre-existing rows. NULL/"" on legacy single-tenant rows — the
-- tenant-authz read filter treats those as "no tenant" (admin-visible).

ALTER TABLE runs ADD COLUMN IF NOT EXISTS tenant_id TEXT;

UPDATE runs
   SET tenant_id = s.tenant_id
  FROM sessions s
 WHERE s.id = runs.session_id
   AND (runs.tenant_id IS NULL OR runs.tenant_id = '');

CREATE INDEX IF NOT EXISTS runs_by_tenant_active
    ON runs(tenant_id, status) WHERE tenant_id IS NOT NULL;
