-- 0094_schedule_run_state_claim.up.sql — RFC DZ: claim each slot once.
--
-- The sweeper used to list due rows and advance next_run_at only AFTER the
-- run, so every replica with the scheduler enabled fired the same rows, and a
-- run longer than its cadence left its row due. Now a fire first CLAIMS its
-- slot with a compare-and-set on next_run_at; these columns record the slot
-- the last claim took, which replica took it, and when. All nullable: a row
-- never claimed has none.

ALTER TABLE schedule_run_state ADD COLUMN slot_at    TIMESTAMPTZ;
ALTER TABLE schedule_run_state ADD COLUMN claimed_by TEXT;
ALTER TABLE schedule_run_state ADD COLUMN claimed_at TIMESTAMPTZ;
