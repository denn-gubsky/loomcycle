-- RFC H Decision 10 "Layer 2" durable dedup. A run may carry an
-- optional idempotency_key; a second CreateRun with the same key is
-- refused at the unique index rather than spawning a duplicate. The
-- webhook spawn path sets idempotency_key = delivery_id so a redelivery
-- that survives past the in-memory Layer-1 TTL — or lands on a different
-- replica — still dedups durably.
--
-- NULLable + a partial unique index (WHERE idempotency_key IS NOT NULL)
-- so the vast majority of runs (no key) are unconstrained and the index
-- stays small. Not a secret (the delivery id is opaque, safe to persist).
ALTER TABLE runs ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX runs_idempotency_key ON runs (idempotency_key) WHERE idempotency_key IS NOT NULL;
