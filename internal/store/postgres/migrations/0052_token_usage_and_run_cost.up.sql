-- 0052_token_usage_and_run_cost.up.sql — RFC AV: per-scope token-usage & cost
-- attribution (operator vs tenant key).
--
-- token_usage is the append-only per-CALL ledger (one row per LLM call) beneath
-- the per-run summary on runs. Its load-bearing column is credential_source
-- (operator|tenant|user, + credential_scope_id): when a tenant runs on the
-- operator's key the tokens are the operator's bill AND the tenant's
-- consumption; when a tenant's own key (RFC AR override) pays, only the tenant's.
-- The operator bill (source=operator) and each tenant's consumption/self-funded
-- spend then fall out of the SAME rows — two queries over one flag, no
-- double-entry. No secrets: token counts, provider/model, the owning scope id
-- (already non-secret, like user_id) and the computed/provider-reported cost.
--
-- runs gains the per-run cost + credential-source SUMMARY: the durable record
-- (the ledger is prunable). cost is NULL when unpriced (model absent from the
-- pricing table), distinct from a genuine zero cost (mock / code-js).

CREATE TABLE token_usage (
    id                    BIGSERIAL   PRIMARY KEY,
    run_id                TEXT        NOT NULL,
    session_id            TEXT,
    tenant_id             TEXT        NOT NULL DEFAULT '',
    user_id               TEXT,
    agent_id              TEXT,
    parent_run_id         TEXT,
    iteration             INTEGER     NOT NULL DEFAULT 0,
    provider              TEXT        NOT NULL,
    model                 TEXT        NOT NULL,
    credential_source     TEXT        NOT NULL,
    credential_scope_id   TEXT        NOT NULL DEFAULT '',
    input_tokens          BIGINT      NOT NULL DEFAULT 0,
    output_tokens         BIGINT      NOT NULL DEFAULT 0,
    cache_creation_tokens BIGINT      NOT NULL DEFAULT 0,
    cache_read_tokens     BIGINT      NOT NULL DEFAULT 0,
    -- DOUBLE PRECISION (not NUMERIC): cost is a derived, display-oriented
    -- summary figure, so a clean float scan across both backends (sqlite REAL)
    -- is worth more than arbitrary-precision decimal here. NULL ⇒ unpriced.
    cost                  DOUBLE PRECISION,
    cost_currency         TEXT,
    ts                    TIMESTAMPTZ NOT NULL
);
CREATE INDEX token_usage_by_run    ON token_usage (run_id);
CREATE INDEX token_usage_tenant_ts ON token_usage (tenant_id, ts);
CREATE INDEX token_usage_source_ts ON token_usage (credential_source, ts);

ALTER TABLE runs ADD COLUMN cost                DOUBLE PRECISION;
ALTER TABLE runs ADD COLUMN cost_currency       TEXT;
ALTER TABLE runs ADD COLUMN credential_source   TEXT;
ALTER TABLE runs ADD COLUMN credential_scope_id TEXT;
