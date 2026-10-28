-- 0122_device_gate_repair.up.sql — apply 0118 on a database that skipped it.
--
-- WHY THIS EXISTS (it looks redundant next to 0118 — it is not):
--
-- 0121 (teamdef_active.promoter_acl_owner, #2209) merged to main BEFORE 0118
-- (the device gate, #2205). golang-migrate keeps one monotonic version and
-- applies only migrations strictly ABOVE it, so a database that migrated on
-- the commit in between sits at 121 and will never run 0118: its
-- device_authorizations and tenant_auth_policies tables would be missing,
-- and device login and per-tenant gate policies would fail on it. This
-- migration is a version above 121 that does what 0118 does. Every statement
-- is IF NOT EXISTS, so on a database that did run 0118 (any database first
-- migrated after #2205) it changes nothing.
--
-- Keep it identical to 0118's schema; 0118 stays the owner of the tables.

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
