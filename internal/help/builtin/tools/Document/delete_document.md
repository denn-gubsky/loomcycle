---
name: Document/delete_document
description: "Document op=delete_document — permanently delete a document, every chunk in it, their edges and history, and its Path-tree names."
---
`delete_document` removes a whole document: every chunk, every edge touching
those chunks (including links from other documents), tags, images, body history,
and every Path-tree name pointing at it. It cannot be undone. To remove only
part of a document, use `delete_chunk`; to correct a fact, use
`supersede_chunk`.

## Arguments

- `id` (or `document_id`) — the **document** id (not a chunk id). Or:
- `path` — the document's Path-tree name. Used only when `id` is empty.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{deleted: true, document_id, n_chunks_deleted}`. `n_chunks_deleted` counts the
root too. A `document_id` that matched nothing returns `n_chunks_deleted: 0`.

## Errors

- `delete_document: missing required field: document_id (or id, or path)`.
- `delete_document: no such path: /...` — nothing is named there in this scope.
- `an agent may not modify the ontology document ...` — the tenant ontology can
  only be changed by an operator. Retrying is pointless.

## Examples

Delete a document by path:

```json
{"op": "delete_document", "path": "/docs/old-draft"}
```

```json result
{"deleted": true, "document_id": "b4407b522dc11495d3de371311db17f0", "n_chunks_deleted": 12}
```

Delete an agent-scope document by id:

```json
{"op": "delete_document", "id": "b4407b522dc11495d3de371311db17f0", "scope": "agent"}
```
