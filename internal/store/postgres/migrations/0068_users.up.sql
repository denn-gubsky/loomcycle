-- 0068_users.up.sql — the tenant-owned first-class `users` table (RFC BX
-- Phase 2 / P2a).
--
-- Until now a "user" in loomcycle was DERIVED: it existed only as the
-- distinct set of runs.user_id values (see directory.go / ListUsers). There
-- was no row to name, so a tenant operator could not register a subject
-- ahead of its first run, give it a display name, disable it, or mark it as
-- sandboxed. This table makes user identity first-class and tenant-owned so
-- a substrate:tenant operator can manage its OWN tenant's users.
--
-- access_mode is the RFC BX binary collaboration dial:
--   'tenant'   = full whole-tenant collaboration (every subject in the
--                tenant shares its workspace) — today's behaviour, hence the
--                DEFAULT, so nothing changes for existing deployments;
--   'isolated' = the subject is sandboxed to its own scope.
-- status is the lifecycle flag ('active' | 'disabled'). Neither is enforced
-- here (P2a is the table + CRUD only); auth/minting integration is a later PR.
--
-- '' = the shared/operator/legacy tenant, matching runs/sessions/channels.
-- The PK is (tenant_id, subject) so two tenants can each own a same-named
-- subject and every read is tenant-confinable.
--
-- IF NOT EXISTS keeps the migration re-runnable: the migration test suite
-- rewinds the version pointer and replays MigrateUp, so each migration must
-- no-op cleanly on a second pass (mirrors 0066/0067).

CREATE TABLE IF NOT EXISTS users (
    tenant_id    TEXT        NOT NULL DEFAULT '',
    subject      TEXT        NOT NULL,
    display_name TEXT        NOT NULL DEFAULT '',
    access_mode  TEXT        NOT NULL DEFAULT 'tenant',   -- 'tenant' | 'isolated'
    status       TEXT        NOT NULL DEFAULT 'active',    -- 'active' | 'disabled'
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT,
    PRIMARY KEY (tenant_id, subject)
);
