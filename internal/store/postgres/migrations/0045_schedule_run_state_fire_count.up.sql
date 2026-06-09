-- 0045_schedule_run_state_fire_count.up.sql — RFC S / F36: lifetime
-- fire-count for the max_fires self-retiring schedule.
--
-- schedule_run_state is the sweeper's per-def runtime view. This adds a
-- monotonic counter the sweeper bumps on every real fire (any status) so
-- a schedule with ScheduledRun.max_fires > 0 auto-retires after its Nth
-- fire. Existing rows backfill to 0 via the column DEFAULT (unbounded,
-- the pre-RFC-S behavior).

ALTER TABLE schedule_run_state ADD COLUMN fire_count BIGINT NOT NULL DEFAULT 0;
