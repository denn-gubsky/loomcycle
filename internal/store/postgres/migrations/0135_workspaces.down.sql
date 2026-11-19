-- 0135_workspaces.down.sql — drop RFC GW P1's workspace rows. The worker's
-- directories are not touched; a worker reporting them at its next attach
-- finds them unknown and is told to remove them.
DROP TABLE IF EXISTS workspaces;
