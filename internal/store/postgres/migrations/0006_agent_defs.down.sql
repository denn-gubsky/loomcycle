-- 0006_agent_defs.down.sql — drop the v0.8.5 substrate def-storage.
-- Order matters: agent_def_active references agent_defs.
DROP INDEX IF EXISTS agent_defs_by_run;
DROP INDEX IF EXISTS agent_defs_by_parent;
DROP INDEX IF EXISTS agent_defs_by_name;
DROP TABLE IF EXISTS agent_def_active;
DROP TABLE IF EXISTS agent_defs;
