-- 0002_memory.up.sql — v0.8.0 Memory tool storage.
--
-- One row per (scope, scope_id, key). value is JSONB so list/get can
-- round-trip the stored shape and a future query primitive can use the
-- @> / -> operators without a migration. expires_at is the TTL anchor;
-- NULL means "no expiry".
--
-- The PRIMARY KEY (scope, scope_id, key) doubles as the lookup index
-- for get/incr/delete. The partial expires_at index keeps the sweep
-- DELETE cheap (small index, only the rows that actually have a TTL).

CREATE TABLE memory (
    scope       TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL,
    key         TEXT        NOT NULL,
    value       JSONB       NOT NULL,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, scope_id, key)
);

CREATE INDEX memory_by_expires_at ON memory(expires_at) WHERE expires_at IS NOT NULL;
