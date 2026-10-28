-- 0122_device_gate_repair.down.sql — DELIBERATE NO-OP.
--
-- The up migration creates the device-gate tables only where 0118 was
-- skipped. Nothing records which migration created them, so dropping them
-- here would destroy them on every database where 0118 owns them. Down past
-- 122 therefore leaves them in place; down past 118 drops them as before.
--
-- golang-migrate requires the file to exist; an empty statement body is a
-- valid, successful migration.
SELECT 1;
