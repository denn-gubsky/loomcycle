-- 0126_passkeys_scim.down.sql — drop the RFC FA P6 tables. Nothing
-- references them by FK.
ALTER TABLE auth_removals DROP COLUMN IF EXISTS prior_status;
ALTER TABLE auth_removals DROP COLUMN IF EXISTS stage;
DROP INDEX IF EXISTS scim_group_members_by_subject;
DROP TABLE IF EXISTS scim_group_members;
DROP TABLE IF EXISTS scim_groups;
DROP INDEX IF EXISTS scim_users_live_subject;
DROP INDEX IF EXISTS scim_users_live_name;
DROP TABLE IF EXISTS scim_users;
DROP TABLE IF EXISTS webauthn_ceremonies;
DROP INDEX IF EXISTS user_passkeys_by_subject;
DROP TABLE IF EXISTS user_passkeys;
