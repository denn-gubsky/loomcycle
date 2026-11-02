-- 0128_dirents_document_index.up.sql — RFC FQ: the reverse of a dirent.
--
-- A dirent names a document (resource_ref.document_id); nothing answered
-- "which names does this document have" without scanning the whole tree. The
-- scope-wide snapshot and the document access check ask it per page, so it is
-- indexed. An expression index: no column, so no writer (DirentCreate, Move,
-- snapshot restore) has to keep anything current. sqlshared.direntDocumentID
-- spells the same expression; the two must match for the planner to use it.

CREATE INDEX IF NOT EXISTS dirents_document_id
    ON dirents (tenant_id, scope, scope_id, (resource_ref->>'document_id'))
    WHERE kind = 'document';
