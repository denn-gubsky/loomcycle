-- 0017_skill_defs.up.sql — v0.8.22 SkillDef substrate.
--
-- Mirror of 0006_agent_defs against skills. The store stays
-- content-agnostic — the `definition` column carries the JSON-
-- encoded skill body + metadata (body / description /
-- allowed_tools); the tool layer at internal/tools/builtin/skilldef.go
-- owns the schema for that JSON.
--
-- Invariants mirrored from agent_defs:
--   * append-only (only INSERT + UPDATE of `retired`)
--   * (name, version) is UNIQUE and monotonic per name
--   * parent_def_id chains lineage; empty for v1 / bootstrap rows
--   * BootstrappedFromStatic=TRUE marks the v1 row created from a
--     LOOMCYCLE_SKILLS_ROOT filesystem entry by SkillDef.fork
--
-- Indexes:
--   skill_defs_by_name    — covers ListByName + GetByNameVersion
--   skill_defs_by_parent  — partial; covers ListChildren only when
--                           parent_def_id is set (most rows are v1)
--   skill_defs_by_run     — partial; covers provenance queries
CREATE TABLE skill_defs (
    def_id                    TEXT        PRIMARY KEY,
    name                      TEXT        NOT NULL,
    version                   INTEGER     NOT NULL,
    parent_def_id             TEXT        REFERENCES skill_defs(def_id),
    definition                JSONB       NOT NULL,
    description               TEXT,
    created_at                TIMESTAMPTZ NOT NULL,
    created_by_agent_id       TEXT,
    created_by_run_id         TEXT,
    retired                   BOOLEAN     NOT NULL DEFAULT FALSE,
    bootstrapped_from_static  BOOLEAN     NOT NULL DEFAULT FALSE,
    UNIQUE(name, version)
);

CREATE INDEX skill_defs_by_name   ON skill_defs(name, version DESC);
CREATE INDEX skill_defs_by_parent ON skill_defs(parent_def_id) WHERE parent_def_id IS NOT NULL;
CREATE INDEX skill_defs_by_run    ON skill_defs(created_by_run_id) WHERE created_by_run_id IS NOT NULL;

-- The active pointer: at most one promoted version per name.
-- Mirrors agent_def_active 1:1.
CREATE TABLE skill_def_active (
    name                  TEXT        PRIMARY KEY,
    def_id                TEXT        NOT NULL REFERENCES skill_defs(def_id),
    promoted_at           TIMESTAMPTZ NOT NULL,
    promoted_by_agent_id  TEXT
);
