-- 0109_acl.down.sql — drop the RFC EY access-control tables. Nothing else
-- references them by FK; the users table (0068) and token rows are left as
-- they were.
DROP TABLE IF EXISTS acl_version;
DROP INDEX IF EXISTS acl_bindings_by_subject;
DROP TABLE IF EXISTS acl_bindings;
DROP INDEX IF EXISTS user_group_members_by_member;
DROP TABLE IF EXISTS user_group_members;
DROP TABLE IF EXISTS user_groups;
DROP TABLE IF EXISTS acl_defs;
