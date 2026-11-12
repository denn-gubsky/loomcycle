-- 0134_tenant_telemetry_content_capture.down.sql — drop RFC GG P3's content
-- capture settings and the record of when content was exported.
DROP TABLE IF EXISTS tenant_telemetry_content_exports;
ALTER TABLE tenant_telemetry_policies DROP COLUMN IF EXISTS content_max_chars;
ALTER TABLE tenant_telemetry_policies DROP COLUMN IF EXISTS content_capture;
