DROP INDEX IF EXISTS operator_token_defs_by_expires_at;
ALTER TABLE operator_token_defs DROP COLUMN IF EXISTS expires_at;
