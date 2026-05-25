-- v0.12.1 multi-replica HA Phase 2: cluster-wide per-user concurrency counter.
--
-- One row per user_id that has at least one active run across the cluster.
-- active_count is atomically incremented at run-admission and decremented at
-- run-completion by whichever replica owns the run. The CHECK constraint
-- prevents underflow; the operator's MaxConcurrentRunsPerUser config is the
-- cap, NOT a per-row value (cap-decrease mid-flight is handled naturally:
-- existing slots stay; new acquires see the new cap on the next TryAcquire).
--
-- Only populated when LOOMCYCLE_REPLICA_ID is set — single-replica
-- deployments leave this table untouched and use the in-memory perUser map
-- on Semaphore (v0.10.1 behavior, byte-identical).
--
-- Phase 5's replica TTL sweeper will reap orphaned slots when a replica
-- crashes without releasing. Until then, a crashed replica leaks at most
-- MaxConcurrentRunsPerUser slots per affected user until manual intervention
-- or process restart.

CREATE TABLE IF NOT EXISTS user_quotas (
    user_id      TEXT        PRIMARY KEY,
    active_count INTEGER     NOT NULL DEFAULT 0 CHECK (active_count >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
