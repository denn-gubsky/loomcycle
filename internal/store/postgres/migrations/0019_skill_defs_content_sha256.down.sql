DROP INDEX IF EXISTS skill_defs_by_content_sha256;
ALTER TABLE skill_defs DROP COLUMN IF EXISTS content_sha256;
