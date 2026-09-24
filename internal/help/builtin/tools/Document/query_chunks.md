---
name: Document/query_chunks
description: "Document op=query_chunks — list chunks by structure (document, path, type, status, parent, tag), or run a read-only SQL SELECT over the chunk tables."
---
`query_chunks` finds chunks by their STRUCTURE — which document, which
subtree, which type, status or tag. It returns the chunk rows without bodies.
Use it when you know what you are filtering on. It is not a text search: to
find chunks by what their bodies SAY, use `search`; to follow links from a
chunk you already have, use `backlinks` or `related`.

**The filters combine with AND, and a raw `sql` ignores all of them.**

## Arguments

All optional; with none you get the first 100 chunks of the scope.

- `document_id` — only chunks of this document.
- `under_path` — only chunks of documents named at or under this Path-tree
  path, e.g. `/docs/specs`.
- `type` — only chunks of this type. The filter also matches the more
  specific subtypes of it; the result then carries `type_expanded_to`.
- `status` — exact match, e.g. `open`.
- `parent_id` — only the direct children of this chunk.
- `tag` — only chunks carrying exactly this tag.
- `tag_prefix` — chunks tagged with this tag or anything nested under it:
  `area` matches `area` and `area/billing`.
- `limit` — rows to return, default 100. A value over 1000 is treated as 100.
- `sql` — a single read-only `SELECT` (or `WITH … SELECT`) run as-is. Tables:
  `documents`, `chunks` (id, document_id, parent_id, position, type, status,
  title, revision, created_at, updated_at), `chunk_edges` (from_id, to_id,
  kind, auto), `chunk_tags` (chunk_id, tag), `document_tags`. Chunk BODIES are
  not in these tables — get them with `get_chunk`. Write plain SQL with literal
  values; there is no way to pass parameters.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

With filters: `{chunks: [{id, document_id, parent_id, title, type, status,
position, revision}], type_expanded_to?}`, ordered by document, parent,
position. Empty fields are omitted. Fetch a body with
`{"op":"get_chunk","id":...}`.

With `sql`: `{columns: [...], rows: [[...], ...], truncated}`. `truncated:
true` means the server's row cap cut the result.

## Errors

- An empty `chunks` list is not an error — nothing matched in THIS scope.
- `query_chunks: ... read-only statements ...` — `sql` must be a SELECT. Use
  `update_chunk`, `delete_chunk` and the other ops to change data.
- `query_chunks: ... only one SQL statement per call ...` — remove the `;`.
- `query_chunks: no such table ...` or a syntax error — fix the SQL; retrying
  it unchanged fails the same way.

## Examples

The open tasks in one document:

```json
{"op": "query_chunks", "document_id": "b4407b522dc11495d3de371311db17f0", "type": "task", "status": "open"}
```

```json result
{"chunks": [{"id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "document_id": "b4407b522dc11495d3de371311db17f0",
  "parent_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "title": "Migrate the database", "type": "task",
  "status": "open", "position": 0, "revision": 3}]}
```

Everything tagged under `billing` in the documents below `/docs`:

```json
{"op": "query_chunks", "under_path": "/docs", "tag_prefix": "billing", "limit": 50}
```

A title search with SQL (the filters above cannot match on title):

```json
{"op": "query_chunks", "sql": "SELECT id, document_id, title FROM chunks WHERE title LIKE '%rollback%' LIMIT 20"}
```

```json result
{"columns": ["id", "document_id", "title"], "rows": [["c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "b4407b522dc11495d3de371311db17f0", "Rollback plan"]], "truncated": false}
```
