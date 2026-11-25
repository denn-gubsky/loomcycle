-- 0142_workspace_templates.up.sql — RFC GW P6: workspace templates (§5.5).
--
-- One row per template a tenant saved: its scope (user or tenant), the
-- author's user id in scope_id for a user template ('' for a tenant one), the
-- toolchains and engine of the worker it was saved on, and the SHA-256 digest
-- and size of its archive. The archive itself is a file under the data
-- directory named by id; the row never holds content. Mirrors the SQLite
-- volumes section.
CREATE TABLE IF NOT EXISTS workspace_templates (
    id          TEXT        NOT NULL UNIQUE,
    tenant_id   TEXT        NOT NULL DEFAULT '',
    scope       TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL DEFAULT '',
    name        TEXT        NOT NULL,
    toolchains  TEXT        NOT NULL DEFAULT '[]',
    engine      TEXT        NOT NULL DEFAULT '',
    digest      TEXT        NOT NULL,
    bytes       BIGINT      NOT NULL,
    author_user TEXT        NOT NULL DEFAULT '',
    author_run  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, scope, scope_id, name)
);
