-- RFC CL phase 1 — observed time on a key/value memory row.
--
-- When the remembered thing was SAID or written, as distinct from created_at (when
-- loomcycle stored it, which on a bulk import is one clustered instant for the whole
-- corpus and answers no question anyone asks).
--
-- NULLABLE on purpose and with no backfill: a row nobody dated has no observed time,
-- and inventing one — from created_at, or by parsing a date out of the value text —
-- would be worse than none. A wrong observed_at silently filters the right row out of
-- a window it belongs in, which is the failure this column exists to remove.
ALTER TABLE memory ADD COLUMN IF NOT EXISTS observed_at TIMESTAMPTZ;

-- Partial: observed_at is NULL on every undated row, and on a corpus that was never
-- dated that is all of them. Indexing those pays for the majority to serve a query
-- that never asks about them — the same reasoning as memory_by_source_session (0065).
CREATE INDEX IF NOT EXISTS memory_by_observed_at
    ON memory (tenant_id, scope, scope_id, observed_at)
    WHERE observed_at IS NOT NULL;
