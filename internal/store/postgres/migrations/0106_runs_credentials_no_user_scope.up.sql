-- Persist whether a run may resolve a stored credential ($cred:) in its
-- user's own scope. A trigger whose definition's author could not act as
-- the run's user (a webhook, whose user can come from the delivery; a
-- schedule written for someone else) starts its run with the user scope
-- withheld. Resume rebuilds a run's identity from its row, with no trigger
-- to re-derive the bit from, so it rides the row like operator_key_restricted
-- and isolated. Additive default-false column; legacy rows read false.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS credentials_no_user_scope BOOLEAN NOT NULL DEFAULT FALSE;
