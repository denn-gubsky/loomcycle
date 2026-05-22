-- 0019_skill_defs_content_sha256.up.sql — v0.9.x content signing
-- for skill_defs. Mirror of 0018_agent_defs_content_sha256 — same
-- column shape, same NULL semantics, same partial-index strategy.
-- See 0018 for the architectural rationale.

ALTER TABLE skill_defs ADD COLUMN content_sha256 TEXT;

CREATE INDEX skill_defs_by_content_sha256
    ON skill_defs(content_sha256)
    WHERE content_sha256 IS NOT NULL;
