-- 0107_webhook_deliveries_claim.up.sql — a dedup_key claim's identity.
--
-- A webhook with a dedup_key claims its event's keys before its run exists,
-- and records the run when it registers. A claim whose run never did held the
-- event for the whole lifetime, and a takeover by age alone could start a
-- second run of an event whose run had in fact started. So each claim gets:
--   claim_id     a fencing token, the same on every key of one claim: the run
--                is linked only while it is current, and a takeover issues a
--                new one, so a setup that outlived its claim cannot link;
--   claimed_at   when it was made, for the takeover's grace;
--   run_key      the idempotency_key and delivery_alt_key its run carries,
--   run_alt_key  which find a run that registered without being linked.
--
-- Additive: '' / NULL on every existing row, which is never taken over for its
-- age (a channel delivery's key, or a claim made before this).
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS claim_id TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS run_key TEXT NOT NULL DEFAULT '';
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS run_alt_key TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS webhook_deliveries_by_claim_id ON webhook_deliveries (claim_id) WHERE claim_id <> '';
