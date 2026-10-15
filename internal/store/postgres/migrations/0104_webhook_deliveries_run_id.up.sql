-- 0104_webhook_deliveries_run_id.up.sql — the run a held delivery key started.
--
-- A webhook with a dedup_key holds its keys here, not on the run row, for as
-- long as the definition says. A duplicate is answered as one unless the run
-- the key started ended failed: a run lost in a restart must not keep the
-- forge's redelivery from being handled. So each key records that run.
--
-- Additive: '' on every existing row, which starts no run (a channel or a
-- team's own webhook), and on a key whose run has not registered yet.
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS run_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS webhook_deliveries_by_run_id ON webhook_deliveries (run_id) WHERE run_id <> '';
