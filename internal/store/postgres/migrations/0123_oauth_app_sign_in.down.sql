DROP INDEX IF EXISTS operator_token_defs_by_oauth_client;
DROP INDEX IF EXISTS operator_token_defs_by_oauth_family;
ALTER TABLE operator_token_defs DROP COLUMN IF EXISTS oauth_family;
ALTER TABLE operator_token_defs DROP COLUMN IF EXISTS oauth_client;
DROP TABLE IF EXISTS oauth_refresh_tokens;
DROP TABLE IF EXISTS oauth_refresh_families;
DROP TABLE IF EXISTS oauth_codes;
