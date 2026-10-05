-- 0092_team_webhook_arms.up.sql — a team's own webhooks, armed by running walks.
--
-- A team may declare webhooks of its own; each answers only while a walk of
-- the team runs. One row per (tenant, team, webhook, walk run) records that a
-- walk armed it, so any replica can accept a delivery rather than only the
-- one running the walk. A row is a lease: the walk renews expires_at while it
-- runs and deletes its rows when it ends; a row whose lease has lapsed (a
-- crashed walk's) or whose walk run is no longer running is treated as absent
-- by the receiver, and the next arm of the team drops lapsed rows.
--
-- The primary key serves both reads: the receiver's (tenant, team, name)
-- lookup and a walk's (tenant, team, walk_run_id) delete. Non-secret: the
-- webhook's auth is read from the team version def_id names. Runtime
-- liveness, not configuration; snapshots do not carry it.
--
-- IF NOT EXISTS: the memory_embeddings repair test rewinds the version pointer
-- and replays every later migration, so a bare CREATE would fail `already
-- exists` on that replay.
CREATE TABLE IF NOT EXISTS team_webhook_arms (
    tenant_id   TEXT        NOT NULL DEFAULT '',
    team        TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    walk_run_id TEXT        NOT NULL,
    def_id      TEXT        NOT NULL,
    user_id     TEXT        NOT NULL DEFAULT '',
    armed_at    TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, team, name, walk_run_id)
);
