-- Reverses 0063. Dropping the column loses producer attribution on every row
-- that carried it; the queue itself and its payloads are unaffected.
ALTER TABLE memory_pending DROP COLUMN IF EXISTS origin;
