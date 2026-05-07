-- 0001_init.down.sql — drops the schema introduced by 0001_init.up.sql.
--
-- Order matters: events references runs, runs references sessions, so we
-- drop in reverse FK order. Each DROP TABLE cascades to its indexes;
-- explicit DROP INDEX statements would be redundant and could mask a
-- mismatch with the up-migration if someone added a new index without
-- updating both sides.

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS runs;
DROP TABLE IF EXISTS sessions;
