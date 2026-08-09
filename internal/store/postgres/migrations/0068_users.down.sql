-- 0068_users.down.sql — drop the tenant-owned users table. Owned data
-- (the subjects' runs / sessions / memory) lives in other tables with no FK
-- to here, so dropping this table removes only the identity records.
DROP TABLE IF EXISTS users;
