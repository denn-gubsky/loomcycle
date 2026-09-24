---
name: Document/query_documents
description: "Document op=query_documents — list documents in a scope, filtered by Path subtree, type, status or tag, sorted by title."
---
`query_documents` lists the documents in one scope, optionally filtered, sorted
by title. Use it to find a document when you know its kind or tag but not its
id. It returns documents, not chunks: to search inside them, use `search` or
`query_chunks`.

## Arguments

- `scope` — `user` (default), `agent` or `tenant`.
- `under_path` — only documents named at or under this Path-tree path.
- `type` — only documents of exactly this type (the root chunk's type).
- `status` — only documents with exactly this status.
- `tag` — only documents carrying exactly this tag (a document tag, not a chunk
  tag).
- `limit` — 1 to 1000; default 100. **A value above 1000 is not capped — it
  falls back to 100.**

Filters combine with AND. With no filter you get every document in the scope,
up to `limit`.

## Returns

`{documents: [{document_id, title, root_chunk_id, created_at, updated_at,
type?, status?}]}`. Timestamps are unix nanoseconds.

## Errors

- An empty `documents` list is not an error: nothing in THIS scope matched.
  Try the other scope before concluding the document does not exist.
- `query_documents: invalid path segment ...` — `under_path` is not a valid
  path.

## Examples

Every draft plan the user has:

```json
{"op": "query_documents", "type": "plan", "status": "draft"}
```

```json result
{"documents": [{"document_id": "b4407b522dc11495d3de371311db17f0", "title": "Launch plan", "root_chunk_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "type": "plan", "status": "draft", "created_at": 1772323200000000000, "updated_at": 1772409600000000000}]}
```

Tenant runbooks tagged for the support team:

```json
{"op": "query_documents", "scope": "tenant", "under_path": "/runbooks", "tag": "ops/support", "limit": 20}
```
