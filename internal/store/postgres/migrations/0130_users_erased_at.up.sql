-- 0130_users_erased_at.up.sql — RFC FY Part F: an erasure ends the account.
--
-- A subject erasure deletes the user's sign-in rows (identities, password,
-- TOTP, passkeys, app sign-ins, ...) and keeps the users row as a tombstone:
-- display name cleared, status 'disabled', erased_at set. The row stays so
-- the subject id is never registered again and audit records keep a
-- referent; no store write moves a row with erased_at set out of that
-- state. NULL = a live user (every row before this migration).
ALTER TABLE users ADD COLUMN IF NOT EXISTS erased_at TIMESTAMPTZ;
