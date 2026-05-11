-- v0.8.2 user_tier marker rollback. Drops the column unconditionally —
-- callers reverting this migration accept losing the per-run tier
-- marker for compliance / cost analytics.
ALTER TABLE runs DROP COLUMN IF EXISTS user_tier;
