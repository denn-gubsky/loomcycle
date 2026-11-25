-- 0144_workspace_durable.down.sql — drop RFC GW P12's durable name index.
-- Durable rows are removed first: without the index a later durable open
-- could make a second row of a name, and parked rows (owner_kind 'parked')
-- would be read by an older runtime as an unknown owner kind.
DELETE FROM workspaces WHERE lifetime = 'durable';
DROP INDEX IF EXISTS workspaces_durable_name;
