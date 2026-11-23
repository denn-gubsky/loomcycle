-- 0137_egress_route_defs.up.sql — RFC GW P4: the egress routes a tenant
-- writes with EgressRouteDef (§6.3). Flat (tenant_id, name) rows like
-- volume_defs: no version, no content hash. A route names an upstream URL,
-- the methods a workspace may send to it and, optionally, a stored
-- credential ("$cred:<name>") the core adds on the way out; it never holds a
-- secret. methods is a JSON list as text, read whole.

CREATE TABLE IF NOT EXISTS egress_route_defs (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    name        TEXT        NOT NULL,
    url         TEXT        NOT NULL,
    methods     TEXT        NOT NULL,
    credential  TEXT        NOT NULL DEFAULT '',
    auth        TEXT        NOT NULL DEFAULT '',
    header      TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, name)
);
