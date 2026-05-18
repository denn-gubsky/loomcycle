-- 0013_snapshots.up.sql — v0.8.17 Pause/Resume/Snapshot (Phase 1, PR 2).
--
-- The snapshots table holds full runtime-state snapshots (paused runs +
-- their transcripts, Memory rows, Channel state, AgentDef lineage,
-- Evaluations) serialised as a single JSON envelope per row. See
-- doc-internal/rfcs/pause-resume-snapshot.md § "Wire surface" for the
-- envelope schema; doc-internal/rfcs/semantic-memory.md § "Snapshot
-- integration" for the Memory section's optional embedding field.
--
-- Columns:
--   id            human-readable snapshot ID ("snap_<unix_ms>_<8hex>")
--   created_at    wall-clock at capture (timestamptz; UTC convention
--                 enforced at the Go layer via UTC()).
--   label         operator-supplied free-text label; optional.
--   schema_version envelope's outer version. Bumped only when a
--                 structurally breaking change lands; per-section
--                 schemas evolve independently via the additive-fields
--                 rule (see pause-resume-snapshot RFC).
--   byte_size     size of json_content in bytes; surfaced in list
--                 responses so operators see snapshot sizes without
--                 downloading the payload.
--   json_content  the full envelope as JSONB. JSONB lets future
--                 introspection queries (e.g. "list snapshots whose
--                 paused_runs section is non-empty") work without
--                 deserialising the full payload at the Go layer.
CREATE TABLE snapshots (
    id             TEXT PRIMARY KEY,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    label          TEXT,
    schema_version INTEGER NOT NULL,
    byte_size      BIGINT NOT NULL,
    json_content   JSONB NOT NULL
);

-- The list endpoint orders newest-first; this index makes that O(log N)
-- regardless of how many snapshots accumulate. (Operators may keep
-- hundreds of snapshots from long-running experiments; the index keeps
-- the listing fast.)
CREATE INDEX snapshots_created_at_desc_idx ON snapshots(created_at DESC);
