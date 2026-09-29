-- 0086_runs_walk_id.up.sql — the team walk a run is a member of, as a column.
--
-- A member's walk already rides in runs.parent_context (walk_id), but that is
-- unindexed TEXT JSON: listing one walk's runs meant reading a user's whole
-- run list and filtering it, capped at 100 rows. This column carries the same
-- value — written in the same INSERT as parent_context, from the same struct —
-- so a walk's runs are one index range. NULL for every run that is not a walk
-- member, including the walk's own run (its id IS the walk id).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS walk_id TEXT;

-- Backfill the members written before this column existed. parent_context is
-- TEXT, so the jsonb cast of a value that is not valid JSON would abort the
-- whole migration; each row is cast in its own block and one that fails is
-- left NULL (such a row has no readable lineage either). Postgres 14 has no
-- IS JSON predicate to filter them up front. The LIKE keeps the loop to rows
-- that can carry a walk at all.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT id, parent_context FROM runs
             WHERE walk_id IS NULL AND parent_context LIKE '%"walk_id"%'
    LOOP
        BEGIN
            UPDATE runs SET walk_id = NULLIF(r.parent_context::jsonb ->> 'walk_id', '')
             WHERE id = r.id;
        EXCEPTION WHEN invalid_text_representation THEN
            NULL;
        END;
    END LOOP;
END $$;

-- ListRunsByWalk reads a walk's members in (started_at, id) order and pages
-- by that pair. Partial: only walk members carry the column.
CREATE INDEX IF NOT EXISTS runs_by_walk ON runs (walk_id, started_at, id) WHERE walk_id IS NOT NULL;
