-- v0.12.5 Phase 6 cluster-wide hook registry (migration renumbered
-- to 0026 during rebase because Phase 4's runtime_state took 0025).
--
-- Hook registrations made via POST /v1/hooks land here in cluster
-- mode. Each replica boots and loads the table into an in-memory
-- cache; the loomcycle.hook backplane topic carries create/delete
-- events so every replica's cache stays current.
--
-- Single-replica deployments don't touch this table — the v0.11.x
-- in-process hooks.Registry runs unchanged.
--
-- agents/tools stored as JSONB so future admin queries can filter
-- by selector content; today the in-process cache reads everything
-- and the DB is only durability.

CREATE TABLE IF NOT EXISTS hooks (
    id                 TEXT        PRIMARY KEY,
    owner              TEXT        NOT NULL,
    name               TEXT        NOT NULL,
    phase              TEXT        NOT NULL,
    agents             JSONB       NOT NULL DEFAULT '[]'::jsonb,
    tools              JSONB       NOT NULL DEFAULT '[]'::jsonb,
    callback_url       TEXT        NOT NULL,
    fail_mode          TEXT        NOT NULL DEFAULT 'open',
    timeout_ms         INTEGER     NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_replica TEXT
);

CREATE INDEX IF NOT EXISTS hooks_by_owner ON hooks(owner);
