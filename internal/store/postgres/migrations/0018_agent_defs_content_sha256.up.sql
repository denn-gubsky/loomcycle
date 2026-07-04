-- 0018_agent_defs_content_sha256.up.sql — v0.9.x content signing.
--
-- Adds a deterministic SHA-256 hash of every agent_def's content-
-- bearing fields (system_prompt, tools, skills, max_tokens,
-- max_iterations, etc.) computed by internal/agents.Sign. Operators
-- with Docker-bundled agents use this to ask "is my bundle's source
-- file identical to what's deployed?" without fetching the full
-- Definition JSONB and diffing field-by-field.
--
-- The column is NULLABLE: rows created BEFORE this migration won't
-- have a hash until the boot-time backfill (see
-- internal/store/migrations/backfill_content_sha256.go) walks them
-- on next startup. Rows created AFTER this migration get the hash
-- populated at AgentDef set/fork time. NULL is therefore a
-- transient state, not a permanent shape.
--
-- The index is partial (WHERE content_sha256 IS NOT NULL) because
-- the NULL set during the upgrade window can be large but is never
-- queried by content_sha256 — the verify op falls through to
-- equality check against an empty string and returns matches=false,
-- which is the correct answer.

ALTER TABLE agent_defs ADD COLUMN content_sha256 TEXT;

CREATE INDEX agent_defs_by_content_sha256
    ON agent_defs(content_sha256)
    WHERE content_sha256 IS NOT NULL;
