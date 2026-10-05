-- 0093_webhook_deliveries.up.sql — durable dedup for webhooks that start no run.
--
-- A webhook delivery that spawns a run is deduped durably by the run's
-- idempotency_key / delivery_alt_key. A team's own webhook publishes a channel
-- message instead, so nothing durable remembered it: the per-process cache was
-- its only replay guard, and once any replica could accept a team's webhook, a
-- captured signed delivery replayed to another replica (or after a restart)
-- published again. This table holds each accepted delivery's dedup keys (the
-- receiver's, already scoped to the webhook) until expires_at, shared by every
-- replica. The insert IS the claim: of two replicas racing on one delivery,
-- the unique key lets exactly one publish. Non-secret (opaque dedup keys).
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- and replays every later migration, so bare statements would fail `already
-- exists` on that replay.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    delivery_key TEXT        PRIMARY KEY,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS webhook_deliveries_by_expires_at ON webhook_deliveries (expires_at);
