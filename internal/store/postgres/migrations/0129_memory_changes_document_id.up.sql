-- 0129_memory_changes_document_id.up.sql — RFC FV: the document a change belongs to.
--
-- A document change row named its chunk only. A reader-facing feed decides
-- access per DOCUMENT, and resolving the document when the row is read fails
-- for a deletion — the chunk row is gone by then. So the Document tool stamps
-- the document at write time and the CDC decorator records it here. Existing
-- rows read back '' (no document known), which the reader feed never sends.
ALTER TABLE memory_changes ADD COLUMN IF NOT EXISTS document_id TEXT NOT NULL DEFAULT '';
