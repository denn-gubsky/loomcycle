-- 0090_runs_delivery_alt_key.up.sql — a run's second durable dedup key.
--
-- A webhook delivery can have two identities: the sender's delivery id and
-- what its signature covers (the body, or the signed timestamp + body). The
-- signature does not cover the delivery-id header, so dedup must match on
-- either identity, durably and on every replica. idempotency_key holds one;
-- this column holds the other. NULL on legacy rows and on runs with a single
-- key.
--
-- Partial unique, like runs_idempotency_key: two replicas racing on one
-- delivery both miss the lookup, and the index lets only one CreateRun win.
-- Not a secret (opaque, scoped dedup keys).
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- and replays every later migration, so bare statements would fail `already
-- exists` on that replay.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS delivery_alt_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS runs_delivery_alt_key ON runs (delivery_alt_key) WHERE delivery_alt_key IS NOT NULL;
