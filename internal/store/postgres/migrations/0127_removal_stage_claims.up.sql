-- 0127_removal_stage_claims.up.sql — RFC FA P6 review follow-up: the
-- columns that let removal writes exclude each other.
--
-- auth_removals.stage is the stage the sweeper has claimed and is running
-- ('unbinding' | 'erasing'), '' in between: while it is set nobody
-- re-enables the user, so a re-enable never races an unbind or an erasure.
-- auth_removals.prior_status is the user's status when the removal disabled
-- it ('pending' carries an approval still owed through a re-enable), ''
-- when unknown (a row from before, or a user disabled already).
-- scim_users.reactivated_at is when the SCIM gate last claimed a re-enable
-- of the user: re-enables are counted per window against the removal
-- ceiling, and a user re-enabled inside the window is left out of the
-- percentage base.
--
-- Additive with defaults, so existing rows read back as before ('' and
-- NULL); IF NOT EXISTS keeps the migration re-runnable.
ALTER TABLE auth_removals ADD COLUMN IF NOT EXISTS stage TEXT NOT NULL DEFAULT '';
ALTER TABLE auth_removals ADD COLUMN IF NOT EXISTS prior_status TEXT NOT NULL DEFAULT '';
ALTER TABLE scim_users ADD COLUMN IF NOT EXISTS reactivated_at TIMESTAMPTZ;
