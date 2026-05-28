-- 0029_schedule_defs.up.sql — v1.x RFC E Scheduled Agent Runs storage.
--
-- Three tables. schedule_defs is the append-only versioned-definition
-- layer (yaml `scheduled_runs:` entries remain the immutable root in
-- cfg.ScheduledRuns; this is the DERIVED layer of per-user forks).
-- schedule_def_active is the per-name "which version is current"
-- pointer. schedule_run_state tracks the sweeper's runtime view of
-- last/next per def.
--
-- Same dual-table shape as agent_defs / agent_def_active (v0.8.5)
-- for the same reasons:
--   - Partial-unique indexes for "one active per name" diverge
--     between SQLite and Postgres syntax.
--   - Promote/rollback is a one-row UPDATE here vs a two-row UPDATE
--     flipping a flag. Symmetric.
--
-- definition is JSONB so future ops (find-similar-forks, etc.) can
-- use @> operators without a migration. Same shape AgentDef uses.

CREATE TABLE schedule_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES schedule_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX schedule_defs_by_name   ON schedule_defs(name, version DESC);
CREATE INDEX schedule_defs_by_parent ON schedule_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX schedule_defs_by_run    ON schedule_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

CREATE TABLE schedule_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES schedule_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);

-- schedule_run_state is the sweeper's view of last/next per def.
-- One row per active schedule; FK + ON DELETE CASCADE so retiring a
-- def via DELETE (rare; usually retired flag) auto-cleans state.
--
-- last_run_at / last_run_id / last_status / last_error: bookkeeping
-- for `GET /v1/_scheduledef list` + the eventual /ui/schedules.
-- next_run_at: the sweeper's primary index (WHERE next_run_at <= now).
-- paused_until: runtime-pause (`POST /v1/_schedules/{name}/pause`)
-- escape valve. NULL = not runtime-paused.
CREATE TABLE schedule_run_state (
    def_id          TEXT        PRIMARY KEY REFERENCES schedule_defs(def_id) ON DELETE CASCADE,
    last_run_at     TIMESTAMPTZ,
    last_run_id     TEXT,
    last_status     TEXT,
    last_error      TEXT,
    next_run_at     TIMESTAMPTZ NOT NULL,
    paused_until    TIMESTAMPTZ
);

CREATE INDEX schedule_run_state_due ON schedule_run_state(next_run_at);
