-- 0001_init.up.sql — initial schema for the Postgres Store adapter.
--
-- Mirrors the SQLite final schema (after v0.4 ALTERs) but uses native
-- Postgres types: TIMESTAMPTZ for time, BIGINT for token counts (32-bit
-- INTEGER was always too small for cumulative usage at scale), BIGSERIAL
-- for the events seq, BYTEA for raw payload bytes.
--
-- Why no cumulative ALTER history (unlike sqlite.go's migrate()): Postgres
-- has no v0.3 install base to be back-compat with — the adapter ships with
-- the v0.4 column set already in place. Future schema changes go in
-- 0002_*.sql, 0003_*.sql etc. via golang-migrate's numbered migrations.

CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    agent       TEXT NOT NULL,
    user_id     TEXT,                        -- nullable; v0.4+ caller-supplied
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX sessions_by_user ON sessions(user_id) WHERE user_id IS NOT NULL;

CREATE TABLE runs (
    id                       TEXT PRIMARY KEY,
    session_id               TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    status                   TEXT NOT NULL,
    started_at               TIMESTAMPTZ NOT NULL,
    completed_at             TIMESTAMPTZ,
    stop_reason              TEXT,
    input_tokens             BIGINT NOT NULL DEFAULT 0,
    output_tokens            BIGINT NOT NULL DEFAULT 0,
    cache_creation_tokens    BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens        BIGINT NOT NULL DEFAULT 0,
    model                    TEXT,
    error                    TEXT,
    -- v0.4 tracking + cancel fields. All nullable for parity with the
    -- SQLite adapter's "added later, may be NULL on legacy rows" shape.
    agent_id                 TEXT,
    parent_agent_id          TEXT,
    parent_run_id            TEXT,
    user_id                  TEXT,
    last_heartbeat_at        TIMESTAMPTZ
);

CREATE INDEX runs_by_session         ON runs(session_id);
CREATE INDEX runs_by_agent_id        ON runs(agent_id)        WHERE agent_id        IS NOT NULL;
CREATE INDEX runs_by_parent_agent_id ON runs(parent_agent_id) WHERE parent_agent_id IS NOT NULL;
CREATE INDEX runs_by_user_active     ON runs(user_id, status) WHERE user_id        IS NOT NULL;

CREATE TABLE events (
    seq         BIGSERIAL PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id      TEXT NOT NULL REFERENCES runs(id)     ON DELETE CASCADE,
    ts          TIMESTAMPTZ NOT NULL,
    type        TEXT NOT NULL,
    payload     BYTEA NOT NULL
);

CREATE INDEX events_by_session ON events(session_id, seq);
