-- 0111_password_gate.up.sql — RFC FA P2: the login → user mapping every auth
-- gate shares, and the password gate's credentials, second factors,
-- invitations, resets and throttle counters.
--
-- Nothing here is replayable from a dump of the database:
--   * a password is an Argon2id PHC string whose digest is HMAC-peppered
--     with a key held outside the database (pepper_kid names which one, so a
--     rotation is detected at the next login and the password re-hashed);
--   * a TOTP secret is a credential.Sealed envelope under the deployment key,
--     bound to the row's (tenant_id, subject);
--   * invite, reset and recovery codes are stored only as hex SHA-256 hashes;
--     the code itself is shown once and never written.
--
-- user_identities is keyed on (gate, issuer, external_subject) with no tenant
-- in the key: a login form asks for no tenant, so a login name is unique
-- across the deployment. UNIQUE (tenant_id, subject, gate) holds "one
-- identity per gate per user" and also serves the by-subject lookup.
--
-- auth_throttle stores window_end rather than the window length: callers
-- pass the window per call, and a read must know when the current window
-- closed without being told its length.
--
-- No FK to users: a user delete removes these rows explicitly
-- (AuthSubjectDelete), matching how acl_* and runs treat users. '' = the
-- shared/operator tenant, as everywhere else. IF NOT EXISTS keeps the
-- migration re-runnable.

CREATE TABLE IF NOT EXISTS user_identities (
    tenant_id        TEXT        NOT NULL DEFAULT '',
    subject          TEXT        NOT NULL,
    gate             TEXT        NOT NULL,
    issuer           TEXT        NOT NULL DEFAULT '',
    external_subject TEXT        NOT NULL,
    email            TEXT        NOT NULL DEFAULT '',
    email_verified   BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at    TIMESTAMPTZ,
    PRIMARY KEY (gate, issuer, external_subject),
    UNIQUE (tenant_id, subject, gate)
);

CREATE TABLE IF NOT EXISTS user_credentials (
    tenant_id    TEXT        NOT NULL DEFAULT '',
    subject      TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    phc          TEXT        NOT NULL,
    pepper_kid   TEXT        NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    failed_count INTEGER     NOT NULL DEFAULT 0,
    locked_until TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, subject, kind)
);

CREATE TABLE IF NOT EXISTS user_totp (
    tenant_id    TEXT        NOT NULL DEFAULT '',
    subject      TEXT        NOT NULL,
    sealed       TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at TIMESTAMPTZ,
    last_step    BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, subject)
);

CREATE TABLE IF NOT EXISTS user_recovery_codes (
    tenant_id  TEXT        NOT NULL DEFAULT '',
    subject    TEXT        NOT NULL,
    code_hash  TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    used_at    TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, subject, code_hash)
);

CREATE TABLE IF NOT EXISTS auth_invites (
    invite_id        TEXT        PRIMARY KEY,
    code_hash        TEXT        NOT NULL UNIQUE,
    gate             TEXT        NOT NULL,
    tenant_id        TEXT        NOT NULL DEFAULT '',
    subject          TEXT        NOT NULL DEFAULT '',
    access_mode      TEXT        NOT NULL DEFAULT '',
    groups           TEXT        NOT NULL DEFAULT '[]',
    login            TEXT        NOT NULL DEFAULT '',
    email            TEXT        NOT NULL DEFAULT '',
    return_to        TEXT        NOT NULL DEFAULT '',
    bootstrap        BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by       TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    redeemed_at      TIMESTAMPTZ,
    redeemed_subject TEXT        NOT NULL DEFAULT '',
    revoked_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS auth_invites_by_gate_tenant ON auth_invites (gate, tenant_id);

CREATE TABLE IF NOT EXISTS auth_resets (
    reset_id   TEXT        PRIMARY KEY,
    code_hash  TEXT        NOT NULL UNIQUE,
    gate       TEXT        NOT NULL,
    tenant_id  TEXT        NOT NULL DEFAULT '',
    subject    TEXT        NOT NULL,
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS auth_resets_by_subject ON auth_resets (tenant_id, subject);

-- A tenant that has ever had a user. Written before the first users row of a
-- tenant by every path that creates one (POST /v1/_users, an invite
-- redemption, a snapshot restore — all through the store), and never
-- deleted. A website's auth:manage credential may give a tenant its owner
-- only while the tenant has no marker AND no users rows, so deleting every
-- user cannot re-arm that bootstrap and hand the tenant to a new owner. A
-- legacy cloud tenant (tokens, never a users row) has no marker and can be
-- bootstrapped exactly once: that is how its owner is migrated.
CREATE TABLE IF NOT EXISTS tenant_bootstraps (
    tenant_id TEXT        PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    via       TEXT        NOT NULL DEFAULT ''
);
-- Tenants that already have users count as bootstrapped from the start.
INSERT INTO tenant_bootstraps (tenant_id, via)
    SELECT DISTINCT tenant_id, 'backfill' FROM users
    ON CONFLICT (tenant_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS auth_throttle (
    throttle_key  TEXT        PRIMARY KEY,
    failures      INTEGER     NOT NULL DEFAULT 0,
    window_start  TIMESTAMPTZ NOT NULL,
    window_end    TIMESTAMPTZ NOT NULL,
    blocked_until TIMESTAMPTZ
);
