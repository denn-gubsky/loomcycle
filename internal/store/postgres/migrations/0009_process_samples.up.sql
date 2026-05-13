-- 0009_process_samples.up.sql — v0.8.x process-resource metrics sampler.
--
-- New table for periodic process-resource samples (RSS, Go heap,
-- goroutine count, CPU%) captured while at least one agent run is
-- active. Time-series shape — one row per sample tick; populated
-- by internal/metrics/sampler.go.
--
-- The Linux-only fields (loomcycle_rss_bytes, loomcycle_cpu_pct_x100)
-- store 0 on non-Linux platforms (build-tag-split in the sampler).
-- system_* columns are NULLable; only populated when the operator
-- sets LOOMCYCLE_METRICS_COLLECT_SYSTEM=1.
--
-- No foreign keys to `runs` — this is a time-series; correlation
-- with a specific run is done at query time via the API endpoint
-- (`GET /v1/_metrics/runs/{run_id}`) which joins on the
-- (started_at, completed_at) window of the runs row.
--
-- Index: process_samples_by_sampled_at drives BOTH the window-scan
-- read path (`/v1/_metrics/samples?since&until`) AND the sweeper
-- DELETE (rows older than retention cutoff).

CREATE TABLE process_samples (
    sample_id                  TEXT        PRIMARY KEY,
    sampled_at                 TIMESTAMPTZ NOT NULL,
    active_runs                INTEGER     NOT NULL,
    queued_runs                INTEGER     NOT NULL,
    loomcycle_rss_bytes        BIGINT      NOT NULL DEFAULT 0,
    loomcycle_heap_alloc_bytes BIGINT      NOT NULL DEFAULT 0,
    loomcycle_heap_inuse_bytes BIGINT      NOT NULL DEFAULT 0,
    loomcycle_num_goroutines   INTEGER     NOT NULL DEFAULT 0,
    loomcycle_cpu_pct_x100     INTEGER     NOT NULL DEFAULT 0,
    system_cpu_pct_x100        INTEGER,
    system_mem_used_mb         INTEGER,
    system_mem_available_mb    INTEGER
);

CREATE INDEX process_samples_by_sampled_at ON process_samples(sampled_at);
