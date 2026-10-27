-- 0118_auth_replay.down.sql — drop the RFC FA P3 replay table.
DROP INDEX IF EXISTS auth_replay_by_expiry;
DROP TABLE IF EXISTS auth_replay;
