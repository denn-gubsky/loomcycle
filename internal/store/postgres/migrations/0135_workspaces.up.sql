-- 0135_workspaces.up.sql — RFC GW P1: workspace rows.
--
-- A workspace is a working directory on one worker. The row is what every
-- replica routes an op by, and owner_run_id is the pin: only the run that
-- opened it may use it, and it is released when THAT run ends (not when its
-- run tree does, which is how ephemeral_volume_defs is keyed).
--
-- state is opening | ready | releasing | lost, moved by compare-and-set in
-- the store so two replicas cannot both move one row. The values are checked
-- in Go, not by a CHECK constraint, so a later phase adding one needs no
-- migration. kind and lifetime are general / ephemeral in P1; git (P11) and
-- durable (P12) workspaces add values, not columns.
--
-- The unique (owner_run_id, name) key is also the lookup for the run's own
-- reads and the sweeper's join to runs. (worker_id, state) serves a worker's
-- capacity count and its reconciliation at attach; (tenant_id, state) the
-- per-tenant limit. Mirrors the SQLite volumes section.

CREATE TABLE IF NOT EXISTS workspaces (
    id             TEXT        PRIMARY KEY,
    tenant_id      TEXT        NOT NULL DEFAULT '',
    user_id        TEXT        NOT NULL DEFAULT '',
    name           TEXT        NOT NULL,
    worker_id      TEXT        NOT NULL,
    kind           TEXT        NOT NULL DEFAULT 'general',
    lifetime       TEXT        NOT NULL DEFAULT 'ephemeral',
    engine         TEXT        NOT NULL DEFAULT '',
    owner_run_id   TEXT        NOT NULL,
    owner_agent_id TEXT        NOT NULL DEFAULT '',
    root_run_id    TEXT        NOT NULL DEFAULT '',
    state          TEXT        NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL,
    UNIQUE (owner_run_id, name)
);
CREATE INDEX IF NOT EXISTS workspaces_by_worker ON workspaces (worker_id, state);
CREATE INDEX IF NOT EXISTS workspaces_by_tenant ON workspaces (tenant_id, state);
