-- 0134_tenant_telemetry_content_capture.up.sql — RFC GG P3: a tenant's opt-in
-- content capture.
--
-- content_capture ('off' | 'on') and content_max_chars (the length each
-- captured field is cut to; 0 = the runtime's default) on the policy row,
-- off for every row before. tenant_telemetry_content_exports records when
-- each tenant's captured content was first and last exported, so an erasure
-- report can say what left for the tenant's own backend, where loomcycle
-- cannot erase it. It outlives a policy delete on purpose. Additive.
ALTER TABLE tenant_telemetry_policies ADD COLUMN IF NOT EXISTS content_capture TEXT NOT NULL DEFAULT 'off';
ALTER TABLE tenant_telemetry_policies ADD COLUMN IF NOT EXISTS content_max_chars INTEGER NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS tenant_telemetry_content_exports (
    tenant_id TEXT        PRIMARY KEY,
    first_at  TIMESTAMPTZ NOT NULL,
    last_at   TIMESTAMPTZ NOT NULL
);
