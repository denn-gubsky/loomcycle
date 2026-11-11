-- tenant_telemetry_policies: where a tenant's own traces are exported, beside
-- (never instead of) the operator's export. One row per tenant; no row = no
-- export to a destination of the tenant's. headers_credential names a
-- tenant-scope credential holding the collector headers; the header values are
-- never stored here. signals is a JSON array ("traces"). Additive; no backfill.
CREATE TABLE IF NOT EXISTS tenant_telemetry_policies (
    tenant_id          TEXT             PRIMARY KEY,
    endpoint           TEXT             NOT NULL,
    protocol           TEXT             NOT NULL,
    headers_credential TEXT             NOT NULL DEFAULT '',
    signals            TEXT             NOT NULL,
    sampler_ratio      DOUBLE PRECISION NOT NULL,
    updated_at         TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_by         TEXT             NOT NULL DEFAULT ''
);
