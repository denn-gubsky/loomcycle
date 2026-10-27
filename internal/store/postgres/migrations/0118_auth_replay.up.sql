-- 0118_auth_replay.up.sql — RFC FA P3: one-time credential values a gate has
-- already accepted, shared by every replica.
--
-- The AWS IAM gate accepts a signed sts:GetCallerIdentity request for five
-- minutes after it was signed. An in-process cache stops a second
-- presentation to the same replica only; this table stops it on all of them.
-- key_hash is the hex SHA-256 of the one-time value (a signature, a jti), so
-- no row holds anything that authenticates. Rows are swept once expired.
--
-- scope keeps gates apart (a value one gate admitted says nothing to
-- another). Not carried in snapshots. IF NOT EXISTS keeps it re-runnable.

CREATE TABLE IF NOT EXISTS auth_replay (
    scope      TEXT        NOT NULL,
    key_hash   TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, key_hash)
);

CREATE INDEX IF NOT EXISTS auth_replay_by_expiry ON auth_replay(expires_at);
