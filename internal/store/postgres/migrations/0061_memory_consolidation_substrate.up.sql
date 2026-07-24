-- 0061_memory_consolidation_substrate.up.sql — RFC BL P2 PR1: the durable
-- substrate for background memory consolidation.
--
-- Three additions, all additive and safe on an existing single-tenant DB:
--
--   1. memory.superseded_at — a soft-archive marker. A consolidated raw memory
--      row is stamped (not deleted): every recall/get/list/search path filters
--      `superseded_at IS NULL`, so a superseded row is invisible to the agent
--      but retained for audit/rollback. NULL = live (every legacy row).
--
--   2. memory_pending — the durable enqueue queue an Add writes to and the
--      consolidator drains. drained_at is a soft-drain marker (idempotent drain
--      + TTL sweeping): a drained row is retained until swept, never re-drained.
--
--   3. memory_cursors — the per-target consolidation watermark + lease. The
--      composite watermark (watermark_completed_at, watermark_session_id)
--      advances monotonically; the lease (leased_by, lease_expires_at) lets one
--      replica own consolidation for a (tenant, scope, scope_id) at a time.

ALTER TABLE memory ADD COLUMN superseded_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS memory_pending (
    id                TEXT        PRIMARY KEY,
    tenant_id         TEXT        NOT NULL DEFAULT '',
    scope             TEXT        NOT NULL,
    scope_id          TEXT        NOT NULL,
    payload           JSONB       NOT NULL,
    source_session_id TEXT,
    source_run_id     TEXT,
    created_at        TIMESTAMPTZ NOT NULL,
    drained_at        TIMESTAMPTZ
);

-- The consolidator drains un-drained rows for one target oldest-first; the TTL
-- sweeper reaps drained rows. Both key on (tenant, scope, scope_id, drained_at).
CREATE INDEX IF NOT EXISTS memory_pending_by_target
    ON memory_pending(tenant_id, scope, scope_id, drained_at);

CREATE TABLE IF NOT EXISTS memory_cursors (
    tenant_id              TEXT        NOT NULL DEFAULT '',
    scope                  TEXT        NOT NULL,
    scope_id               TEXT        NOT NULL,
    watermark_completed_at TIMESTAMPTZ,
    watermark_session_id   TEXT        NOT NULL DEFAULT '',
    leased_by              TEXT        NOT NULL DEFAULT '',
    lease_expires_at       TIMESTAMPTZ,
    updated_at             TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, scope, scope_id)
);
