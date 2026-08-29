-- RFC CL phase 2a — validity interval on a key/value memory row.
--
-- valid_at / invalid_at are when the described thing was TRUE IN THE WORLD, which is
-- a third time distinct from the two already here:
--
--   created_at   when loomcycle stored the row
--   observed_at  when it was SAID          (phase 1)
--   valid_at     when it was TRUE          (this)
--
-- The distinction is not academic. "Yesterday I met artists in Boston", said on
-- October 4, is observed October 4 and valid October 3 — and October 3 is what the
-- question asks about. Measured on LoCoMo, 10 of the 13 date-constrained questions
-- still failing after phase 1 carry exactly this shape of cue.
--
-- HALF-OPEN [valid_at, invalid_at): invalid_at NULL means "still true", matching the
-- bi-temporal entity tier's convention so the two planes answer "what was true then"
-- the same way rather than off by one row.
ALTER TABLE memory ADD COLUMN IF NOT EXISTS valid_at   TIMESTAMPTZ;
ALTER TABLE memory ADD COLUMN IF NOT EXISTS invalid_at TIMESTAMPTZ;

-- Partial for the same reason as memory_by_observed_at (0073): the columns are NULL
-- on every row nobody dated, and on a corpus that was never dated that is all of them.
CREATE INDEX IF NOT EXISTS memory_by_valid_at
    ON memory (tenant_id, scope, scope_id, valid_at)
    WHERE valid_at IS NOT NULL;
