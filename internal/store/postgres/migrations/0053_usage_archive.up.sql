-- 0053_usage_archive.up.sql — RFC AV Phase 2b: the compact usage rollup.
--
-- The per-call token_usage ledger (0052) is high-volume — one row per LLM call.
-- The rollup-and-prune sweeper periodically folds rows older than the detail
-- retention window into this table (one row per period × the full report
-- dimension set) and deletes the raw rows, so token_usage stays bounded while
-- the exact per-(tenant,user,provider,model,source) totals — including the
-- operator-vs-tenant split — are preserved forever at low cardinality.
--
-- period_start is the bucket start (day-truncated ts). The PK is the full
-- dimension tuple so a re-run of the sweeper for the same window folds
-- idempotently (ON CONFLICT adds). user_id is stored '' (not NULL) so it is
-- part of a stable PK. Reports UNION this with the recent token_usage rows.

CREATE TABLE usage_archive (
    period_start          TIMESTAMPTZ      NOT NULL,
    tenant_id             TEXT             NOT NULL DEFAULT '',
    user_id               TEXT             NOT NULL DEFAULT '',
    provider              TEXT             NOT NULL,
    model                 TEXT             NOT NULL,
    credential_source     TEXT             NOT NULL,
    input_tokens          BIGINT           NOT NULL DEFAULT 0,
    output_tokens         BIGINT           NOT NULL DEFAULT 0,
    cache_creation_tokens BIGINT           NOT NULL DEFAULT 0,
    cache_read_tokens     BIGINT           NOT NULL DEFAULT 0,
    cost                  DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_currency         TEXT             NOT NULL DEFAULT '',
    call_count            BIGINT           NOT NULL DEFAULT 0,
    unpriced_calls        BIGINT           NOT NULL DEFAULT 0,
    PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source)
);
CREATE INDEX usage_archive_tenant_period ON usage_archive (tenant_id, period_start);
