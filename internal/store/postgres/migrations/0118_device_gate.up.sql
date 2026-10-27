-- 0118_device_gate.up.sql — RFC FA P5: device login (RFC 8628) and the
-- per-tenant gate policy.
--
-- device_authorizations holds no usable code. device_code_hash is the
-- SHA-256 of a 256-bit device code; user_code_hash is a keyed hash of the
-- short user code under the token pepper, held outside the database (a plain
-- digest of a ~34-bit code would be reversible by enumeration). A row goes
-- pending → approved | denied → redeemed and never back; each step is one
-- conditional UPDATE, so a code is decided once and yields one token. Rows
-- live until expires_at (ten minutes) and are then swept, so a second
-- redemption inside the window reads "already used".
--
-- interval_ms is the RFC 8628 polling interval, lengthened by every poll that
-- comes early; last_poll_early records whether the latest poll did, so one
-- UPDATE ... RETURNING both applies and reports slow_down.
--
-- tenant_auth_policies: a tenant whose people may sign in (and be issued
-- tokens) only through the listed gates. gates is a JSON array of configured
-- gate names, never empty. Admin-managed; the config's auth.tenant_policies
-- is the floor a row can only narrow.
--
-- An approval pins the approving Web UI session: decided_session is its
-- hash, decided_cred_mac binds the credential it was made from (keyed by the
-- device code), and decided_not_after caps the issued token's lifetime at the
-- session's absolute end and that credential's expiry. The redemption
-- re-checks the session and the credential, so no token is issued after
-- either ended.
--
-- No FK: neither table references a user row; a device row names who decided
-- for the approval page and the issued token, and is gone within minutes.

CREATE TABLE IF NOT EXISTS device_authorizations (
    device_code_hash TEXT        PRIMARY KEY,
    user_code_hash   TEXT        NOT NULL UNIQUE,
    client_id        TEXT        NOT NULL,
    source_addr      TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    interval_ms      BIGINT      NOT NULL,
    last_polled_at   TIMESTAMPTZ,
    last_poll_early  BOOLEAN     NOT NULL DEFAULT FALSE,
    status           TEXT        NOT NULL DEFAULT 'pending',
    decided_tenant   TEXT        NOT NULL DEFAULT '',
    decided_subject  TEXT        NOT NULL DEFAULT '',
    decided_gate     TEXT        NOT NULL DEFAULT '',
    decided_at       TIMESTAMPTZ,
    decided_session  TEXT        NOT NULL DEFAULT '',
    decided_cred_mac TEXT        NOT NULL DEFAULT '',
    decided_not_after TIMESTAMPTZ,
    redeemed_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS device_authorizations_by_expiry ON device_authorizations (expires_at);

CREATE TABLE IF NOT EXISTS tenant_auth_policies (
    tenant_id  TEXT        PRIMARY KEY,
    gates      TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT        NOT NULL DEFAULT ''
);
