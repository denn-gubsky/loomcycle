-- 0006_agent_defs.up.sql — v0.8.5 Self-Evolution Substrate storage.
--
-- (No 0005 — that slot was reserved for a v0.8.4 follow-up that
-- shipped inline in 0004 instead. The gap is intentional; golang-
-- migrate applies migrations in numeric order and does not require
-- sequential numbering. Future cherry-picks MUST NOT fill the gap
-- on a deployed database — golang-migrate refuses to apply a
-- migration with a number lower than the database's current version.
-- Use 0009+ for any new migration.)
--
-- Two tables. agent_defs is the append-only versioned-definition
-- layer (operator-blessed static MDs remain the immutable root in
-- cfg.Agents; this is the DERIVED layer of agent-authored versions).
-- agent_def_active is the per-name "which version is current"
-- pointer.
--
-- Why two tables and not is_active BOOL on agent_defs:
--   - Partial-unique indexes for "one active per name" diverge
--     between SQLite and Postgres syntax.
--   - Promote/rollback is a one-row UPDATE here, vs a two-row
--     UPDATE flipping the flag. Symmetric.
--   - Per-tenant active pointers (future v0.9.x) extend the PK to
--     (tenant_id, name); the agent_defs rows stay tenant-agnostic.
--
-- definition is JSONB so a future "find forks similar to this one"
-- query can use @> operators without a migration. Description is
-- free-text "why I made this" — capped in the application layer
-- (LOOMCYCLE_AGENT_DEF_MAX_DESCRIPTION_BYTES, default 8 KB).

CREATE TABLE agent_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES agent_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX agent_defs_by_name   ON agent_defs(name, version DESC);
CREATE INDEX agent_defs_by_parent ON agent_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX agent_defs_by_run    ON agent_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE agent_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES agent_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);
