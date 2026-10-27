-- 0115_auth_external_gates.up.sql — RFC FA P4: the external gates' replay
-- cache and the user removal stage machine.
--
-- auth_assertion_jtis: an external gate's assertion JWT is single-use. The
-- cache must be shared by every replica (an assertion replayed to another
-- replica is refused too), so it is a table, keyed (gate, jti): one gate's
-- jti never collides with another's. Rows live until the assertion could no
-- longer verify (exp + skew, at most a few minutes) and are swept hourly.
-- No secret: a jti is a random label the gate chose.
--
-- auth_removals: §3.7c. A removed user is disabled at once (the row is
-- written with the disable), unbound at unbind_due_at, and erased only at
-- erase_due_at — NULL means an operator must run it, the default. The due
-- times are fixed when the removal starts, so a later config change never
-- makes an old removal erase early. A stage never skips: the sweeper sets
-- unbound_at before it may set erased_at. One removal per user; reactivating
-- the user deletes it. No FK to users, like the other auth tables (the user
-- delete removes it through AuthSubjectDelete).

CREATE TABLE IF NOT EXISTS auth_assertion_jtis (
    gate       TEXT        NOT NULL,
    jti        TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (gate, jti)
);
CREATE INDEX IF NOT EXISTS auth_assertion_jtis_by_expiry ON auth_assertion_jtis (expires_at);

CREATE TABLE IF NOT EXISTS auth_removals (
    tenant_id     TEXT        NOT NULL DEFAULT '',
    subject       TEXT        NOT NULL,
    gate          TEXT        NOT NULL DEFAULT '',
    reason        TEXT        NOT NULL,
    disabled_at   TIMESTAMPTZ NOT NULL,
    unbind_due_at TIMESTAMPTZ NOT NULL,
    erase_due_at  TIMESTAMPTZ,
    unbound_at    TIMESTAMPTZ,
    erased_at     TIMESTAMPTZ,
    created_by    TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, subject)
);
CREATE INDEX IF NOT EXISTS auth_removals_by_unbind_due ON auth_removals (unbind_due_at) WHERE unbound_at IS NULL;
CREATE INDEX IF NOT EXISTS auth_removals_by_erase_due ON auth_removals (erase_due_at) WHERE erased_at IS NULL AND erase_due_at IS NOT NULL;
