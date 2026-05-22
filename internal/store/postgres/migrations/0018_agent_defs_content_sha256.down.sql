DROP INDEX IF EXISTS agent_defs_by_content_sha256;
ALTER TABLE agent_defs DROP COLUMN IF EXISTS content_sha256;
