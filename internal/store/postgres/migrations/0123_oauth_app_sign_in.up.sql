-- 0123_oauth_app_sign_in.up.sql — RFC FL P0: loomcycle as an OAuth 2.0
-- authorization server for its own apps.
--
-- oauth_codes: an authorization code is single-use and lives 60 s. The row
-- stores the code's SHA-256, never the code, and everything it is bound to:
-- the client, the exact redirect_uri, the PKCE challenge, the user and the
-- session that approved it. family_id is written by the exchange that
-- consumes the code, so a second use can revoke what the first produced.
--
-- oauth_refresh_families: one sign-in of one user into one client. Every
-- refresh token of the sign-in belongs to it, and revoking the family ends
-- all of them (reuse detection, RFC 9700 §4.14). scope_cap and restrictions
-- bound every access token the family mints: the signing-in session's
-- scopes and restrictions, so an app never holds more than the session that
-- approved it. A family revoked before it was created (a code replayed while
-- its first exchange was still running) is a tombstone row, which makes the
-- late create fail.
--
-- oauth_refresh_tokens: each refresh token's SHA-256 and whether it was
-- used. A used one presented again is the reuse signal.
--
-- operator_token_defs.oauth_client / oauth_family: an access token an app
-- holds is an ordinary gate-issued user token tagged with the client and
-- the family it came from, so a family revoke retires it and its use is
-- refused once the client is gone.
--
-- No secret is stored: only hashes. No FK to users, like the other auth
-- tables (the user delete removes the rows through AuthSubjectDelete).

CREATE TABLE IF NOT EXISTS oauth_codes (
    code_hash      TEXT        PRIMARY KEY,
    client_id      TEXT        NOT NULL,
    redirect_uri   TEXT        NOT NULL,
    code_challenge TEXT        NOT NULL,
    tenant_id      TEXT        NOT NULL,
    subject        TEXT        NOT NULL,
    gate           TEXT        NOT NULL,
    amr            TEXT        NOT NULL DEFAULT '[]',
    session_hash   TEXT        NOT NULL DEFAULT '',
    scope_cap      TEXT        NOT NULL DEFAULT '[]',
    restrictions   TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    consumed_at    TIMESTAMPTZ,
    family_id      TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS oauth_codes_by_expiry ON oauth_codes (expires_at);
CREATE INDEX IF NOT EXISTS oauth_codes_by_client ON oauth_codes (client_id) WHERE consumed_at IS NULL;

CREATE TABLE IF NOT EXISTS oauth_refresh_families (
    family_id           TEXT        PRIMARY KEY,
    client_id           TEXT        NOT NULL DEFAULT '',
    tenant_id           TEXT        NOT NULL DEFAULT '',
    subject             TEXT        NOT NULL DEFAULT '',
    gate                TEXT        NOT NULL DEFAULT '',
    amr                 TEXT        NOT NULL DEFAULT '[]',
    scope_cap           TEXT        NOT NULL DEFAULT '[]',
    restrictions        TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL,
    last_used_at        TIMESTAMPTZ NOT NULL,
    idle_expires_at     TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    revoked_at          TIMESTAMPTZ,
    revoke_reason       TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_client ON oauth_refresh_families (client_id) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_subject ON oauth_refresh_families (tenant_id, subject);
CREATE INDEX IF NOT EXISTS oauth_refresh_families_by_expiry ON oauth_refresh_families (absolute_expires_at);

CREATE TABLE IF NOT EXISTS oauth_refresh_tokens (
    token_hash TEXT        PRIMARY KEY,
    family_id  TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS oauth_refresh_tokens_by_family ON oauth_refresh_tokens (family_id);

ALTER TABLE operator_token_defs ADD COLUMN IF NOT EXISTS oauth_client TEXT NOT NULL DEFAULT '';
ALTER TABLE operator_token_defs ADD COLUMN IF NOT EXISTS oauth_family TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS operator_token_defs_by_oauth_family ON operator_token_defs (oauth_family) WHERE oauth_family <> '';
CREATE INDEX IF NOT EXISTS operator_token_defs_by_oauth_client ON operator_token_defs (oauth_client) WHERE oauth_client <> '';
