-- 0050_dirents.up.sql — RFC AL Path primitive: the dirent (path tree) substrate.
--
-- Maps a (tenant_id, scope, scope_id, parent_path, name) coordinate to a
-- backing resource (kind + resource_ref jsonb). PK is the full coordinate so
-- each (tenant, scope, scope_id) tree is independent and a name is unique
-- within its parent directory. The PK doubles as the resolve/ls lookup index;
-- parent_path supports one-level (=) and recursive (prefix) listings.

CREATE TABLE dirents (
    tenant_id    TEXT        NOT NULL DEFAULT '',
    scope        TEXT        NOT NULL,
    scope_id     TEXT        NOT NULL DEFAULT '',
    parent_path  TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    resource_ref JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, scope, scope_id, parent_path, name)
);
