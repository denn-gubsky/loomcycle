-- 0091_token_usage_call_timing.down.sql — drop the per-call timing columns.
DROP INDEX IF EXISTS token_usage_timing;
ALTER TABLE token_usage DROP COLUMN IF EXISTS queue_ms;
ALTER TABLE token_usage DROP COLUMN IF EXISTS decode_ms;
ALTER TABLE token_usage DROP COLUMN IF EXISTS prefill_ms;
ALTER TABLE token_usage DROP COLUMN IF EXISTS load_ms;
ALTER TABLE token_usage DROP COLUMN IF EXISTS ttft_ms;
ALTER TABLE token_usage DROP COLUMN IF EXISTS duration_ms;
