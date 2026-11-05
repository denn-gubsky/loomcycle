---
name: Document/copy_documents
description: "Document op=copy_documents — copy a set of documents into another scope (or the same scope under new names), with their history, tags, images and the relations between them."
---
`copy_documents` copies a set of documents from `scope` into `to_scope` on the
server. Each copy gets new ids and the same Path names (rewritten by
`path_map`). The copy carries:
- every chunk with its hierarchy, type, status, fields and validity;
- the bodies and their edit history;
- tags, canvas layout, the document's own types, image bytes and derived
  search units;
- the relations between the copied documents, re-pointed at the copies.

Each copy's root records `copied_from: {scope, document_id}` in its fields, and
each copy is announced on the target scope's `documents/ingested` queue with
reason `copied`. The source is not changed.

A relation from a copied document to one outside the set is not copied: it
stays in the source and is listed under `dangling`. So is a relation whose
evidence quotes a document outside the set. Copy the documents together to keep
the relations between them.

**It is a dry run unless you pass `dry_run: false`.** A dry run checks the whole
set, and reports what would be copied, what is refused and what is left behind.
It writes nothing. Run it first.

## Arguments

- `to_scope` (required) — `agent`, `user` or `tenant`: where the copies go. The
  same scope as `scope` makes duplicates, and then `path_map` is required.
- `scope` — `user` (default), `agent` or `tenant`: where the documents are.
- `document_ids` — the documents to copy, by id.
- `under_path` — copy every document named at or under this path.
- `under_paths` — several subtrees. `document_ids`, `under_path` and
  `under_paths` combine; a document named twice is copied once. At most 500
  documents per call.
- `path_map` — name prefixes to rewrite, from → to, e.g.
  `{"/loomcycle": "/kb/loomcycle"}`. The longest matching prefix wins; a name
  no prefix matches keeps its path.
- `dry_run` — `true` (default) reports without writing; `false` copies.

## Returns

```
{scope, to_scope, dry_run,
 documents: [{id, new_id?, paths}],
 counts: {documents, chunks, statements, revisions, assets, units?},
 refused: [{document_id, code, detail?}],
 dangling: [{statement_id, kind, from, to, outside}], dangling_count,
 body_failures?, name_failures?, statements_failed?, statement_error?}
```

`new_id` appears only on a real copy, and `units` only after one. `outside`
says which part of a dangling relation lies outside the set: `from`, `to` or
`evidence`. `statement_error` is the first failed statement's error. The lists are capped at 200 entries; `dangling_count` and
`refused_count` stay exact.

A refused document is not copied, and the rest of the set goes ahead. The
refusal codes are:
- `not_found` — no such document in `scope`.
- `is_ontology` — the tenant ontology, or a target name that is the
  ontology's. The ontology is edited in place, never copied.
- `is_subject` — a subject document.
- `holds_facts` — a document holding facts (chunks with a natural key).
  Subjects and facts have their own identity, so they don't move between
  scopes this way.
- `forbidden` — the caller's access-control rules don't allow write on a
  target name.

A document the caller's access-control rules don't let it read is left out of
the set before anything is checked. Found under a path, it is not copied,
refused, counted or listed; named in `document_ids`, it is refused as
`not_found`, exactly as an id that names nothing. A relation into it is not
listed under `dangling`.
- `path_collision` — a target name already names something in `to_scope`, or
  two documents of the set would take the same name.
- `key_collision` — `to_scope` already holds the document's work key or
  identity key.
- `write_failed` — the rows could not be written; nothing of that document was
  left behind.

## Errors

- `copy_documents: missing required field: to_scope`.
- `copy_documents: to_scope: ...` — the caller may not use that scope (an agent
  needs both `memory_scopes` and `sql_scopes` for `tenant`).
- `copy_documents: a copy into the same scope needs a path_map`.
- `copy_documents: path_map: ...` — a prefix is not a valid absolute path.
- `copy_documents: no documents named ...` — nothing to copy.
- `copy_documents: the set holds N documents, at most 500 per call` — split it.

## Examples

See what copying a project's documents into the tenant scope would do:

```json
{"op": "copy_documents", "scope": "user", "to_scope": "tenant", "under_path": "/loomcycle"}
```

```json result
{"scope": "user", "to_scope": "tenant", "dry_run": true,
 "documents": [{"id": "2303ed852e036f14acca1a8f2d106506", "paths": ["/loomcycle/rfcs/document-copy-move"]}],
 "counts": {"documents": 1, "chunks": 14, "statements": 0, "revisions": 3, "assets": 0},
 "refused": [], "dangling": [], "dangling_count": 0}
```

Copy them, under a new folder:

```json
{"op": "copy_documents", "scope": "user", "to_scope": "tenant", "under_path": "/loomcycle",
 "path_map": {"/loomcycle": "/kb/loomcycle"}, "dry_run": false}
```
