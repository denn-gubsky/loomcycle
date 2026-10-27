-- 0120_client_defs.up.sql — RFC FL P0b: runtime-registered app clients
-- (ClientDef), beside the config-declared auth.clients.
--
-- A client's name is its OAuth client_id, which travels alone in every
-- authorize request, so a name belongs to ONE tenant across the whole
-- deployment: the store refuses another tenant's create of a name any row
-- holds, retired or not (a retired client's id is never handed to someone
-- else). tenant_id '' is a client an admin registered for every tenant.
--
-- Versioned like the other defs: a change is a new version, the newest
-- unretired version is the one in force, and retiring a client retires
-- every version. content_sha256 is the definition's hash for verify.
-- No secret: a public client has none.

CREATE TABLE IF NOT EXISTS client_defs (
    def_id            TEXT        PRIMARY KEY,
    name              TEXT        NOT NULL,
    tenant_id         TEXT        NOT NULL DEFAULT '',
    version           INTEGER     NOT NULL,
    definition        TEXT        NOT NULL,
    content_sha256    TEXT        NOT NULL,
    description       TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL,
    created_by        TEXT        NOT NULL DEFAULT '',
    created_by_tenant TEXT        NOT NULL DEFAULT '',
    retired_at        TIMESTAMPTZ,
    UNIQUE (name, version)
);
CREATE INDEX IF NOT EXISTS client_defs_by_tenant ON client_defs (tenant_id);
