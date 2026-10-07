-- 0096_schedule_catch_up.up.sql — RFC DZ: catch_up_max.
--
-- After an outage a schedule may run its last catch_up_max missed slots
-- instead of collapsing them into one. Each claim recomputes the backlog from
-- next_run_at, but a slot's KIND must outlive the claims: the last kept slot is
-- the only one still due once the others have run, and recomputing alone would
-- take it for a live slot (which forbid skips and allow starts regardless of
-- the limit). catch_up_until is the newest slot of the current backlog: a
-- claimed slot at or before it is a catch-up slot. NULL = no backlog.
--
-- missed_slots is how many slots the most recent catch-up claim dropped
-- (older than the last catch_up_max, or every one but the latest when
-- catch_up_max is 0), so an outage is never collapsed silently.

ALTER TABLE schedule_run_state ADD COLUMN IF NOT EXISTS catch_up_until TIMESTAMPTZ;
ALTER TABLE schedule_run_state ADD COLUMN IF NOT EXISTS missed_slots   INTEGER NOT NULL DEFAULT 0;
