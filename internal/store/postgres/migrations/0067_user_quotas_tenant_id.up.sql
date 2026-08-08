-- 0067_user_quotas_tenant_id.up.sql — tenant-scope the per-user
-- concurrency counter.
--
-- user_quotas is the cluster-wide per-user admission gate (0024): one
-- row per user_id tracking active+queued runs, capped by the operator's
-- per-user limit. It keyed on the bare user_id, so two tenants whose
-- subjects collide (e.g. both have an "alice") shared ONE counter and
-- ONE cap — tenant A's alice contended with tenant B's alice for the
-- same slots. This adds the tenant axis so the cap is enforced per
-- (tenant, user), matching how runs/sessions are already partitioned.
--
-- '' = the shared/operator/legacy tenant; existing rows backfill to ''
-- via the column DEFAULT (a single-tenant cluster reads exactly as
-- before — every row is '' and the bare-user semantics are preserved).
-- The table is a transient counter (Postgres-cluster-only; the
-- single-replica path uses an in-process map), so there is nothing
-- durable to migrate beyond the in-flight counts.
--
-- IF NOT EXISTS / IF EXISTS keep the migration re-runnable: the test
-- suite rewinds the version pointer and replays MigrateUp, so every
-- migration must no-op cleanly on a second pass (see 0063/0066).

ALTER TABLE user_quotas ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE user_quotas DROP CONSTRAINT IF EXISTS user_quotas_pkey;
ALTER TABLE user_quotas ADD PRIMARY KEY (tenant_id, user_id);
