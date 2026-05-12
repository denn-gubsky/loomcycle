-- 0007_runs_agent_def.up.sql — v0.8.5 runs.agent_def_id audit column.
--
-- NULL = "this run resolved against the static cfg.Agents fallback,
-- no DB-versioned definition." That distinguishes static-resolved
-- runs from DB-resolved runs without a separate flag column. Cost
-- retros + experiment audits facet on this.
--
-- Additive ALTER on an existing table; no row lock in Postgres ≥ 11
-- because the new column has no DEFAULT (NULL is free).

ALTER TABLE runs ADD COLUMN agent_def_id TEXT;
CREATE INDEX runs_by_agent_def ON runs(agent_def_id) WHERE agent_def_id IS NOT NULL;
