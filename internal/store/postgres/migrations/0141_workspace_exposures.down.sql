-- 0141_workspace_exposures.down.sql — drop RFC GW P9's exposures: no
-- workspace port is reachable from outside its run.
DROP TABLE IF EXISTS workspace_exposures;
