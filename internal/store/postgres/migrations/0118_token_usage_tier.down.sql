-- 0118_token_usage_tier.down.sql — drop tier and user_tier.
--
-- The archive's old key has neither, so rows of one day that differ only by
-- tier are folded back into one first (the same sums the rollup takes; the
-- currency is the first non-empty one, as the rollup keeps it). Without the
-- fold the old key could not be restored. The tier split is lost, as it was
-- never recorded before this migration.
ALTER TABLE usage_archive DROP CONSTRAINT IF EXISTS usage_archive_pkey;
WITH old AS (
    DELETE FROM usage_archive RETURNING *
)
INSERT INTO usage_archive (period_start, tenant_id, user_id, provider, model, credential_source, purpose,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
    cost, cost_currency, call_count, unpriced_calls, audio_seconds, input_chars)
SELECT period_start, tenant_id, user_id, provider, model, credential_source, purpose,
    SUM(input_tokens), SUM(output_tokens), SUM(cache_creation_tokens), SUM(cache_read_tokens),
    SUM(cost), COALESCE(MAX(NULLIF(cost_currency, '')), ''), SUM(call_count), SUM(unpriced_calls),
    SUM(audio_seconds), SUM(input_chars)
FROM old
GROUP BY period_start, tenant_id, user_id, provider, model, credential_source, purpose;
ALTER TABLE usage_archive DROP COLUMN IF EXISTS user_tier;
ALTER TABLE usage_archive DROP COLUMN IF EXISTS tier;
ALTER TABLE usage_archive ADD CONSTRAINT usage_archive_pkey
    PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source, purpose);
ALTER TABLE token_usage DROP COLUMN IF EXISTS user_tier;
ALTER TABLE token_usage DROP COLUMN IF EXISTS tier;
