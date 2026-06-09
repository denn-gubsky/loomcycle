-- 0044_webhook_defs_tenant_id.up.sql — RFC N (completion): tenant-scope
-- the Webhook definition plane. Final RFC-N-completion migration
-- (0040–0044) — with this, EVERY content-addressed substrate Def is
-- tenant-isolated.
--
-- A WebhookDef declares an inbound HTTP webhook endpoint. The authoring
-- plane was global-by-name (clobber + leak across tenants). This adds the
-- owning-tenant axis; per-tenant inbound delivery rides the new URL-tenant
-- route POST /v1/_webhooks/{tenant}/{name} (the bare-root
-- POST /v1/_webhooks/{name} resolves under the shared "" tenant, so
-- existing single-tenant webhooks are unaffected). Mirror of 0037, minus
-- the dynamic_* tier (Webhook has none).
--
-- '' = the shared/operator/legacy tenant; existing rows backfill via the
-- column DEFAULT.

ALTER TABLE webhook_defs ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_defs DROP CONSTRAINT webhook_defs_name_version_key;
ALTER TABLE webhook_defs ADD CONSTRAINT webhook_defs_tenant_name_version_key UNIQUE (tenant_id, name, version);

ALTER TABLE webhook_def_active ADD COLUMN tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_def_active DROP CONSTRAINT webhook_def_active_pkey;
ALTER TABLE webhook_def_active ADD PRIMARY KEY (tenant_id, name);
