-- 0008_evaluations.up.sql — v0.8.5 Self-Evolution Substrate evals.
--
-- Pure-insert table; agents call EvaluationSubmit per scored run.
-- def_id is denormalised from runs.agent_def_id at submit time —
-- captures which version the run actually targeted at the moment
-- the eval landed, robust to later promote / retire ops.
--
-- emitter_role is server-derived from ctx + run identity (the
-- store does NOT compute it; the tool layer stamps it before
-- handing the row off). Closed set: 'self' / 'sibling' / 'parent' /
-- 'external' / 'unrelated'. Future v0.9.x experiments can add new
-- role strings without a schema change.
--
-- Score is REAL (Postgres DOUBLE PRECISION) — the RL convention
-- range [-1, 1] OR [0, 1] is enforced in the application layer
-- (operator chooses; substrate is range-agnostic). Dimensions are
-- arbitrary named axes; judgement is free-form JSON. All capped at
-- the application layer (LOOMCYCLE_EVALUATION_MAX_* env vars).
--
-- NO foreign keys on run_id or def_id: evaluations are an immutable
-- audit log and must survive any future run/def pruning. Referential
-- integrity is enforced at the application layer (EvaluationSubmit
-- validates run_id exists before inserting). A RESTRICT FK would
-- block legitimate admin pruning workflows; CASCADE would silently
-- delete audit data. Mirrors the SQLite schema in sqlite.go.

CREATE TABLE evaluations (
    eval_id            TEXT             PRIMARY KEY,
    run_id             TEXT             NOT NULL,
    def_id             TEXT,
    score              DOUBLE PRECISION NOT NULL,
    dimensions         JSONB,
    judgement          JSONB,
    rationale          TEXT,
    emitter_role       TEXT             NOT NULL,
    emitter_agent_id   TEXT,
    emitter_run_id     TEXT,
    created_at         TIMESTAMPTZ      NOT NULL
);

CREATE INDEX evaluations_by_run     ON evaluations(run_id);
CREATE INDEX evaluations_by_def     ON evaluations(def_id) WHERE def_id IS NOT NULL;
CREATE INDEX evaluations_by_emitter ON evaluations(emitter_agent_id) WHERE emitter_agent_id IS NOT NULL;
