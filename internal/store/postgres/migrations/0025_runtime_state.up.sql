-- v0.12.3 multi-replica HA Phase 4: cluster-wide pause/resume state.
--
-- Single-row table (id = 'singleton') holding the cluster's pause
-- state. Every replica reads from here on its iteration-boundary
-- check + run-admission 503 gate (through a 1s in-process cache).
-- Backplane events ('loomcycle.pause' / 'loomcycle.resume') invalidate
-- the cache for sub-second propagation.
--
-- Only used when LOOMCYCLE_REPLICA_ID is set. Single-replica installs
-- ignore this table — Phase 4's pause.Manager only reads/writes
-- when its RuntimeStateStore is non-nil.

CREATE TABLE IF NOT EXISTS runtime_state (
    id                  TEXT        PRIMARY KEY DEFAULT 'singleton',
    state               TEXT        NOT NULL    DEFAULT 'running',
    state_changed_at    TIMESTAMPTZ NOT NULL    DEFAULT now(),
    paused_at           TIMESTAMPTZ,
    paused_runs_count   INTEGER     NOT NULL    DEFAULT 0
);

-- Seed the singleton row so Get never has to handle the "no row" case.
INSERT INTO runtime_state (id) VALUES ('singleton') ON CONFLICT DO NOTHING;
