-- 0083_drop_hooks.down.sql — recreate the table 0026 / 0047 / 0081 built
-- (empty: the registrations it held are not recoverable).
CREATE TABLE IF NOT EXISTS hooks (
    id                 TEXT        PRIMARY KEY,
    owner              TEXT        NOT NULL,
    name               TEXT        NOT NULL,
    phase              TEXT        NOT NULL,
    agents             JSONB       NOT NULL DEFAULT '[]'::jsonb,
    tools              JSONB       NOT NULL DEFAULT '[]'::jsonb,
    callback_url       TEXT        NOT NULL,
    fail_mode          TEXT        NOT NULL DEFAULT 'open',
    timeout_ms         INTEGER     NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_replica TEXT,
    tenant_id          TEXT        NOT NULL DEFAULT '',
    code               TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS hooks_by_owner ON hooks(owner);
CREATE INDEX IF NOT EXISTS hooks_by_tenant ON hooks(tenant_id);
