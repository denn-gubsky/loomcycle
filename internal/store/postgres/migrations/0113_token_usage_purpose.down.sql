-- 0113_token_usage_purpose.down.sql — drop purpose and audio_seconds.
--
-- The archive's old key has no purpose, so rows of one day that differ only
-- by purpose are folded back into one first (the same sums the rollup takes;
-- the currency is the first non-empty one, as the rollup keeps it). Without
-- the fold the old key could not be restored. The minutes are lost, as they
-- were never recorded before this migration.
ALTER TABLE usage_archive DROP CONSTRAINT IF EXISTS usage_archive_pkey;
WITH old AS (
    DELETE FROM usage_archive RETURNING *
)
INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    cost, cost_currency, call_count, unpriced_calls)
SELECT period_start, tenant_id, user_id, provider, model, credential_source,
    SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens),
    SUM(cost), COALESCE(MAX(NULLIF(cost_currency, '')), ''), SUM(call_count), SUM(unpriced_calls)
FROM old
GROUP BY period_start, tenant_id, user_id, provider, model, credential_source;
ALTER TABLE usage_archive DROP COLUMN IF EXISTS audio_seconds;
ALTER TABLE usage_archive DROP COLUMN IF EXISTS purpose;
ALTER TABLE usage_archive ADD CONSTRAINT usage_archive_pkey
    PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source);
ALTER TABLE token_usage DROP COLUMN IF EXISTS audio_seconds;
ALTER TABLE token_usage DROP COLUMN IF EXISTS purpose;
