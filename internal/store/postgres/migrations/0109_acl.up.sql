-- 0109_acl.up.sql — RFC EY access control: user groups, ACL rule lists,
-- bindings, and the per-tenant version the decision engine's cache keys on.
--
-- An ACL is a flat (tenant_id, name) row whose version bumps on every
-- write and whose history is the audit log — nothing resolves an older
-- version (a binding names "the current list"), so the content-addressed
-- fork/promote lifecycle of the agent family is not used. Retire is
-- reversible (a put on a retired name un-retires it with a new version).
--
-- Groups nest (member_kind = 'group'); the engine caps the depth and the
-- store refuses a cycle at write. A binding has its own id so a subject may
-- carry several and one may be retired alone.
--
-- acl_version is one row per tenant, bumped in the same transaction as
-- every write above: "did anything change" is one integer compare.
--
-- '' = the shared/operator tenant (config-declared lists), matching the
-- other def tables. IF NOT EXISTS keeps the migration re-runnable.

CREATE TABLE IF NOT EXISTS acl_defs (
    tenant_id       TEXT        NOT NULL DEFAULT '',
    name            TEXT        NOT NULL,
    version         INTEGER     NOT NULL DEFAULT 1,
    content_sha256  TEXT        NOT NULL,
    catalog_version INTEGER     NOT NULL,
    rules           JSONB       NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      TEXT        NOT NULL DEFAULT '',
    retired_at      TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS user_groups (
    tenant_id    TEXT        NOT NULL DEFAULT '',
    name         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    external_id  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS user_group_members (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    group_name  TEXT        NOT NULL,
    member_kind TEXT        NOT NULL,   -- 'user' | 'group'
    member_id   TEXT        NOT NULL,   -- a subject or a group name
    added_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    added_by    TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, group_name, member_kind, member_id)
);
CREATE INDEX IF NOT EXISTS user_group_members_by_member ON user_group_members (tenant_id, member_kind, member_id);

CREATE TABLE IF NOT EXISTS acl_bindings (
    binding_id   TEXT        PRIMARY KEY,
    tenant_id    TEXT        NOT NULL DEFAULT '',
    acl_name     TEXT        NOT NULL,
    subject_kind TEXT        NOT NULL,   -- 'user' | 'group' | 'token' | 'tenant'
    subject_id   TEXT        NOT NULL DEFAULT '',
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT        NOT NULL DEFAULT '',
    retired_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS acl_bindings_by_subject ON acl_bindings (tenant_id, subject_kind, subject_id) WHERE retired_at IS NULL;

CREATE TABLE IF NOT EXISTS acl_version (
    tenant_id  TEXT        PRIMARY KEY,
    version    BIGINT      NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
