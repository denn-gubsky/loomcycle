-- 0112_oidc_gate.up.sql — RFC FA P1: redirect logins (OIDC, later OAuth),
-- the time a session's person authenticated, and the gate a token was issued
-- by.
--
-- auth_login_states holds one row per started code-flow login, between the
-- redirect to the provider and its callback. The browser holds a 32-byte
-- secret in its login cookie; the PKCE verifier and the OIDC nonce derive from
-- it, and this row stores only SHA-256 hashes (of the state and of the
-- secret), so a database read can neither finish a login nor tie a callback to
-- another browser. A row is consumed once (consumed_at, set by an UPDATE whose
-- WHERE decides the single winner), lives ten minutes, and is deleted by the
-- session sweeper. Starting a login is throttled per client source, so
-- unauthenticated callers cannot fill the table. Never carried by a snapshot.
--
-- ui_sessions.auth_time is when the person last authenticated as the gate
-- reported it (an ID token's auth_time). NULL = unknown, which no
-- re-authentication check counts as recent.
--
-- operator_token_defs.issued_by_gate names the gate that issued a token for a
-- person it verified ('' for an operator- or tenant-minted token).
--
-- users.status gains the value 'pending' (no constraint exists to widen): a
-- gate-provisioned user awaiting an operator's approval.
CREATE TABLE IF NOT EXISTS auth_login_states (
    state_hash        TEXT        PRIMARY KEY,
    gate              TEXT        NOT NULL,
    binding_hash      TEXT        NOT NULL,
    purpose           TEXT        NOT NULL DEFAULT 'login',
    return_to         TEXT        NOT NULL DEFAULT '',
    reauth            BOOLEAN     NOT NULL DEFAULT FALSE,
    link_tenant       TEXT        NOT NULL DEFAULT '',
    link_subject      TEXT        NOT NULL DEFAULT '',
    link_session_hash TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL,
    expires_at        TIMESTAMPTZ NOT NULL,
    consumed_at       TIMESTAMPTZ
);

ALTER TABLE ui_sessions ADD COLUMN IF NOT EXISTS auth_time TIMESTAMPTZ;
ALTER TABLE operator_token_defs ADD COLUMN IF NOT EXISTS issued_by_gate TEXT NOT NULL DEFAULT '';
