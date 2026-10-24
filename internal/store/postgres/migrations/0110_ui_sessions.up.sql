-- 0110_ui_sessions.up.sql — server-side Web UI login sessions (RFC FA §5).
--
-- The Web UI used to keep the raw API bearer in a cookie. It now holds an
-- opaque 32-byte id; this table holds the session, keyed by the SHA-256 of
-- that id, so a read of the table never yields a cookie that authenticates.
--
-- The credential the session came from is not stored: cred_kind + cred_ref
-- say where to find it again, and cred_mac (an HMAC keyed by the session id)
-- proves on each request that it is still the same credential. ua_hash and
-- ip_hash are keyed the same way.
--
-- revoked_at NULL = live. Expired and revoked rows are deleted by the
-- session sweeper. Never carried by a snapshot: a session belongs to one
-- browser on this deployment.
CREATE TABLE IF NOT EXISTS ui_sessions (
    session_hash        TEXT        PRIMARY KEY,
    tenant_id           TEXT        NOT NULL,
    subject             TEXT        NOT NULL,
    gate                TEXT        NOT NULL,
    amr                 TEXT        NOT NULL DEFAULT '[]',
    cred_kind           TEXT        NOT NULL,
    cred_ref            TEXT        NOT NULL DEFAULT '',
    cred_mac            TEXT        NOT NULL,
    ua_hash             TEXT        NOT NULL DEFAULT '',
    ip_hash             TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL,
    last_seen_at        TIMESTAMPTZ NOT NULL,
    idle_expires_at     TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    revoked_at          TIMESTAMPTZ
);
