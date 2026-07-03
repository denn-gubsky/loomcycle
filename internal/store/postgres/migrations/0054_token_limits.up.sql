-- 0054_token_limits.up.sql — RFC AW: per-scope token budgets (soft + hard).
--
-- token_limits is the dynamic, tenant-scoped budget primitive: one calendar-month
-- token ceiling per (operator | tenant | user) scope. A row's absence = unlimited
-- (today's behavior). A NULL soft_limit or hard_limit = that tier unset. No
-- secrets: scope ids (tenant / subject, already non-secret like user_id) and
-- integer amounts.
--
--   operator row: tenant_id='', scope='operator', scope_id=''      (admin-only)
--   tenant   row: tenant_id=X,  scope='tenant',   scope_id=''      (tenant operator)
--   user     row: tenant_id=X,  scope='user',     scope_id=<subject>
--
-- soft/hard are BIGINT nullable (token counts); the enforced total is
-- input+output+cache_creation+cache_read for the scope this calendar month (UTC).

CREATE TABLE token_limits (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    scope       TEXT        NOT NULL,
    scope_id    TEXT        NOT NULL DEFAULT '',
    soft_limit  BIGINT,
    hard_limit  BIGINT,
    updated_at  TIMESTAMPTZ NOT NULL,
    updated_by  TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, scope, scope_id)
);
