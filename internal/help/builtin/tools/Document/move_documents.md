---
name: Document/move_documents
description: "Document op=move_documents — move a set of documents into another scope, keeping every id, with their history, tags, images and the relations between them."
---
`move_documents` moves a set of documents from `scope` into `to_scope` on the
server. Every id stays the same: the document, its chunks and the relations
between moved documents. A reference to any of them still resolves, now in
`to_scope`. The documents keep their Path names (rewritten by `path_map`), and
the source scope no longer holds them.

A move carries what a copy carries:
- every chunk with its hierarchy, type, status, fields and validity;
- the bodies and their edit history;
- tags, canvas layout, the document's own types, image bytes and derived
  search units;
- the relations between the moved documents, with their status, rulings and
  evidence.

**A relation cannot span two scopes.** A relation between a moved document and
one outside the set refuses the move, unless you pass `dangling: "drop"`: then
those relations are deleted with the source. Add the other documents to the
set to keep them.

**It is a dry run unless you pass `dry_run: false`.** A real move also needs
`expect_count`, the `counts.documents` the dry run reported. If the set changed
since the dry run, the move is refused and nothing moves.

The order keeps something whole at every step. The target is written and
checked against the source first; only then is the source removed; the target
is named last. Until it is named, the target scope's `query_documents` and
`graph_snapshot` leave the document out. **If a move is interrupted, run the
same call again**: it finishes what the earlier call started. Each moved
document is announced on the target scope's `documents/ingested` queue with
reason `moved`, and each move is recorded in the audit log.

## Arguments

- `to_scope` (required) — `agent`, `user` or `tenant`: where the documents go.
  It must differ from `scope`; to rename within one scope use the Path tool's
  `mv`.
- `scope` — `user` (default), `agent` or `tenant`: where the documents are.
- `document_ids` — the documents to move, by id.
- `under_path` — move every document named at or under this path.
- `under_paths` — several subtrees. `document_ids`, `under_path` and
  `under_paths` combine; a document named twice moves once. At most 500
  documents per call.
- `path_map` — name prefixes to rewrite, from → to, e.g.
  `{"/loomcycle": "/kb/loomcycle"}`. The longest matching prefix wins; a name
  no prefix matches keeps its path.
- `dangling` — `refuse` (default) or `drop`: what to do with relations between
  a moved document and one outside the set.
- `dry_run` — `true` (default) reports without writing; `false` moves.
- `expect_count` — required with `dry_run: false`: the dry run's
  `counts.documents`.

## Returns

```
{scope, to_scope, dry_run,
 documents: [{id, paths, resumed?}],
 counts: {documents, chunks, statements, revisions, assets, units?},
 refused: [{document_id, code, detail?}],
 dangling: [{statement_id, kind, from, to, outside}], dangling_count,
 blocked_by?, dropped?, to_finish?, moved?, unfinished?,
 body_failures?, name_failures?, statements_failed?, statement_error?}
```

- `blocked_by: "dangling_relations"` — a dry run whose real move would be
  refused for the relations listed under `dangling`.
- `dropped` — the relations deleted with `dangling: "drop"`.
- `to_finish` — a dry run's documents an earlier, interrupted call already
  took out of `scope`; the real call names them.
- `moved` — the ids now in `to_scope`, including those an earlier call left.
- `unfinished` — documents written to `to_scope` but not moved yet, each with
  a `code` (`verify_failed`, `source_not_removed`, `marker_not_cleared`) and a
  `detail`. Their source is kept; run the same call again.
- `resumed` — a document whose target an earlier call of the same move wrote.

A refused document does not move, and the rest of the set goes ahead. The
refusal codes are those of `copy_documents` (`not_found`, `is_ontology`,
`is_subject`, `holds_facts`, `forbidden`, `path_collision`, `key_collision`,
`write_failed`), plus `id_collision`: `to_scope` already holds a document with
that id. Under access control a move needs write on each source document as
well as read.

## Errors

- `move_documents: missing required field: to_scope`.
- `move_documents: the documents are already in scope ...`.
- `move_documents: to_scope: ...` — the caller may not use that scope (an agent
  needs both `memory_scopes` and `sql_scopes` for `tenant`).
- `move_documents: expect_count is required with dry_run false`.
- `move_documents: the set now holds N documents to move, not the M expected`.
- `move_documents: N relations join a document of the set to one outside it`
  — see `dangling`.
- `move_documents: scope ... is held by the lease ...` — a pass is working on
  the source scope; retry when it ends.
- `move_documents: the set holds N documents, at most 500 per call`.

## Examples

See what moving a project's documents into the tenant scope would do:

```json
{"op": "move_documents", "scope": "user", "to_scope": "tenant", "under_path": "/loomcycle"}
```

```json result
{"scope": "user", "to_scope": "tenant", "dry_run": true,
 "documents": [{"id": "2303ed852e036f14acca1a8f2d106506", "paths": ["/loomcycle/rfcs/document-copy-move"]}],
 "counts": {"documents": 1, "chunks": 14, "statements": 0, "revisions": 3, "assets": 0},
 "refused": [], "dangling": [], "dangling_count": 0}
```

Move them:

```json
{"op": "move_documents", "scope": "user", "to_scope": "tenant", "under_path": "/loomcycle",
 "dry_run": false, "expect_count": 1}
```
