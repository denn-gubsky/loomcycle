-- 0091_token_usage_call_timing.up.sql — how long each model call took.
--
-- Per-call timing on the usage ledger, in milliseconds: wall time, time to the
-- first event, and the phases a driver reported (model load, prompt eval,
-- generation, and the wall time the server did not account for). They let a
-- model's throughput be measured and the in-memory estimate be re-seeded at boot.
--
-- Nullable: NULL = not measured (a row from before this migration, or a phase
-- the driver could not see), never "took no time". Nothing is backfilled.
-- Non-secret (durations only).
--
-- The partial index serves the boot seed — the latest timed calls per
-- (provider, model) — and costs nothing for untimed history.
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- and replays every later migration, so bare statements would fail `already
-- exists` on that replay.
ALTER TABLE token_usage ADD COLUMN IF NOT EXISTS duration_ms BIGINT;
ALTER TABLE token_usage ADD COLUMN IF NOT EXISTS ttft_ms BIGINT;
ALTER TABLE token_usage ADD COLUMN IF NOT EXISTS load_ms BIGINT;
ALTER TABLE token_usage ADD COLUMN IF NOT EXISTS prefill_ms BIGINT;
ALTER TABLE token_usage ADD COLUMN IF NOT EXISTS decode_ms BIGINT;
ALTER TABLE token_usage ADD COLUMN IF NOT EXISTS queue_ms BIGINT;
CREATE INDEX IF NOT EXISTS token_usage_timing ON token_usage (provider, model, ts) WHERE duration_ms IS NOT NULL;
