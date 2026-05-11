-- v0.8.2 user_tier marker on runs.
--
-- Captures the user_tier policy applied at run creation so compliance +
-- cost-retrospective queries can facet by tier without grepping logs.
-- Nullable on legacy rows; new rows carry the operator-declared name
-- ("default" / "free" / "low" / "medium" / "high" / ...).
--
-- Additive only — no locking on existing rows, safe to apply against a
-- live database with zero downtime.
ALTER TABLE runs ADD COLUMN user_tier TEXT;
