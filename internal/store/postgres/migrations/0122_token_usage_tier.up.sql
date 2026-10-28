-- 0122_token_usage_tier.up.sql — the model tier and the user tier a usage row
-- was spent under.
--
-- A usage report by tier needs both on the row: tier is the model tier the
-- call was resolved through ('low', 'middle', 'high', …, or 'pinned' for an
-- agent that names its model), user_tier the run's user tier. Neither can be
-- recovered later from the run: the archive keeps no run id. '' is unknown —
-- every row from before this migration, and a call with no agent behind it.
-- Nothing is backfilled.
--
-- The archive keeps the split, so tier and user_tier join its key as purpose
-- did in 0113. Every existing archive row holds '' in both, so the wider key
-- admits them all.
--
-- 0122: 0117, 0118 and 0121 are on main, and 0119 and 0120 are held by open
-- pull requests. golang-migrate keeps one version pointer, so a number below
-- one already applied would be skipped on a database past it.
--
-- Non-secret: two labels from operator configuration.
--
-- IF [NOT] EXISTS throughout: the memory_embeddings repair test rewinds the
-- version pointer and replays every later migration.
ALTER TABLE token_usage   ADD COLUMN IF NOT EXISTS tier      TEXT NOT NULL DEFAULT '';
ALTER TABLE token_usage   ADD COLUMN IF NOT EXISTS user_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_archive ADD COLUMN IF NOT EXISTS tier      TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_archive ADD COLUMN IF NOT EXISTS user_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE usage_archive DROP CONSTRAINT IF EXISTS usage_archive_pkey;
ALTER TABLE usage_archive ADD CONSTRAINT usage_archive_pkey
    PRIMARY KEY (period_start, tenant_id, user_id, provider, model, credential_source, purpose, tier, user_tier);
