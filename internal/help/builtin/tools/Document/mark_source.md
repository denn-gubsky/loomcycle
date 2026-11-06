---
name: Document/mark_source
description: "Document op=mark_source — say whether an imported document's source is still there: present, removed or trashed. Never touches a body."
---
`mark_source` records, on an imported document, whether the place it was
imported from still holds it. A sync that finds an item removed or trashed at
its source marks the document instead of deleting it: its text, statements and
history stay. The status goes into the document's provenance as
`source_status`, and while the source is gone (`removed` or `trashed`) the
document carries the tag `source/removed`, which `query_documents tag=` and
search filters can use. Marking it `present` again removes the tag.

The change is a revision of the document's root chunk: `history` on the root
lists it with the provenance in force at each revision, so an earlier status
stays readable. No body is deleted or rewritten. A re-import keeps the status;
only `mark_source` sets it.

Only an operator, or a run of an agent granted `statement_methods` (`rule` or
`extractor`, the grant `maintain_statement` asks), may call it.

## Arguments

- `id` (required unless `path`) — the **document** id. `document_id` is
  accepted in its place.
- `path` — the document's Path-tree name, in place of `id`.
- `status` (required) — `present`, `removed` or `trashed`.
- `scope` — `user` (default), `agent` or `tenant`: the scope the document is in.

## Returns

`{document_id, source_status, changed, tags}`. `changed` is false when the
document already had that status (no revision is written); `tags` is the
document's tag set afterwards.

## Errors

- `mark_source: permission: ...` — a run without the `statement_methods` grant.
- `mark_source: status "x" is not one of present, removed, trashed`.
- `mark_source: missing required field: document_id (or id, or path)`.
- `mark_source: no such document: <id>`.
- `mark_source: not_imported: ...` — the document was not made by File
  `op=import` or by `import_md` with a source, so it has no source to mark.
- `an agent may not modify the ontology document ...` — refused for the tenant
  ontology.

## Examples

The source item was moved to the trash:

```json
{"op": "mark_source", "scope": "tenant", "id": "b4407b522dc11495d3de371311db17f0", "status": "trashed"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "source_status": "trashed", "changed": true, "tags": ["source/removed", "source/skybits", "untrusted"]}
```

It came back:

```json
{"op": "mark_source", "scope": "tenant", "path": "/sources/field-notes", "status": "present"}
```
