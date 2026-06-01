-- 0035_operator_token_defs.up.sql — RFC L OSS multi-tenant authorization.
--
-- Stores the bearer tokens that replace the single LOOMCYCLE_AUTH_TOKEN
-- shared secret. Each row binds a token to an AUTHORITATIVE PRINCIPAL
-- (tenant_id + subject + allowed_scopes) that the auth middleware stamps
-- into ctx — the principal, not the wire request, drives fairness,
-- memory tenancy, attribution, and audit.
--
-- Deliberately NOT a content-addressed / forkable / versioned substrate
-- Def (unlike memory_backend_defs / webhook_defs): a secret has no
-- meaningful "content hash to fork", and its lifecycle is
-- rotate→grace→retire, not promote. So there is no version column, no
-- *_active pointer, and no parent_def_id — rotation is recorded via
-- rotated_from, and validity via retired_at.
--
-- token_hash = SHA-256(server_pepper ‖ token), hex. The plaintext token
-- is shown to the operator exactly once at create time and NEVER
-- persisted. The hash is indexed (idx_operator_token_hash) so the auth
-- hot path is a single direct lookup compared with the existing
-- subtle.ConstantTimeCompare primitive — NOT a per-request KDF. The
-- pepper (an env-allowlisted var) means a stolen DB dump without the
-- pepper yields no usable lookup.
--
-- VALIDITY: a row authenticates iff retired_at IS NULL OR now < retired_at.
--   create  → retired_at NULL (the current token for its name)
--   rotate  → new row retired_at NULL; the prior row's retired_at set to
--             now + grace (both authenticate during the grace window)
--   retire  → retired_at set to now (immediate invalidation)

CREATE TABLE operator_token_defs (
    def_id               TEXT        PRIMARY KEY,
    name                 TEXT        NOT NULL,
    tenant_id            TEXT        NOT NULL,
    subject              TEXT        NOT NULL,
    token_hash           TEXT        NOT NULL,
    allowed_scopes       JSONB       NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,
    created_by_agent_id  TEXT,
    created_by_run_id    TEXT,
    rotated_from         TEXT        REFERENCES operator_token_defs(def_id),
    retired_at           TIMESTAMPTZ,
    UNIQUE(token_hash)
);

-- The auth hot-path lookup: SHA-256(pepper‖bearer) → row. UNIQUE already
-- creates an index, but name it explicitly for clarity / future EXPLAIN.
CREATE UNIQUE INDEX idx_operator_token_hash ON operator_token_defs(token_hash);

-- List a name's rotation history newest-first; find a name's current
-- (non-retired) token.
CREATE INDEX operator_token_defs_by_name ON operator_token_defs(name, created_at DESC);
