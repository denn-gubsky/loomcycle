---
name: Document/delete_chunk
description: "Document op=delete_chunk — permanently delete a chunk and every chunk beneath it, with their edges, tags and history."
---
`delete_chunk` removes a chunk **and every descendant under it**, together with
their edges (in both directions), tags, images and body history. It cannot be
undone. It refuses a document's root chunk: delete the whole document with
`delete_document`. To correct a fact while keeping the old version for
questions about the past, use `supersede_chunk` instead.

## Arguments

- `id` (required) — the chunk id.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{deleted: true, cascade_deleted_descendants}` — how many chunks under it were
deleted too (0 for a leaf).

## Errors

- `delete_chunk: missing required field: id`.
- `delete_chunk: no such chunk: <id>` — wrong id, or another scope.
- `delete_chunk: refusing to delete a document's root chunk — use delete_document`.
- `delete_chunk: subtree too wide to cascade safely ...` — one level has too
  many children; delete some of the children first, then retry.
- `an agent may not modify the ontology document ...` — operator-only.

## Examples

Delete a section and everything in it:

```json
{"op": "delete_chunk", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00"}
```

```json result
{"deleted": true, "cascade_deleted_descendants": 4}
```

Delete a chunk in an agent-scope document:

```json
{"op": "delete_chunk", "id": "7a6b5c4d3e2f10a9b8c7d6e5f4a3b2c1", "scope": "agent"}
```
