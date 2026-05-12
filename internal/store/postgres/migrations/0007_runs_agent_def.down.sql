DROP INDEX IF EXISTS runs_by_agent_def;
ALTER TABLE runs DROP COLUMN IF EXISTS agent_def_id;
