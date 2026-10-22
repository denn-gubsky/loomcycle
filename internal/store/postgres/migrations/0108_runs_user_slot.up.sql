-- Persist whether a run's admission took a slot in the shared per-user
-- counter (user_quotas). A replica that stops parks its runs with their
-- slots still counted; the resume of such a run gives the slot back once.
-- Without this column the resume gave one back for every top-level run,
-- including runs admitted while the per-user cap was off, and so took back
-- another run's slot. Nullable on purpose: NULL is a row written before the
-- column (parked by an older binary) or restored from a snapshot, for which
-- the resume keeps its earlier rule. Additive; no backfill.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS user_slot BOOLEAN;
