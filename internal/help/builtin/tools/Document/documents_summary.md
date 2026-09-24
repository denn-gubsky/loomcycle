---
name: Document/documents_summary
description: "Document op=documents_summary — title, root type/status and colour settings for many documents in one call, by ids and/or a Path subtree."
---
`documents_summary` returns light metadata for a SET of documents in one call:
the ones you list in `document_ids`, the ones named under `under_path`, or
both. Use it instead of one `get_document` per row when you are labelling a
listing. Unknown ids, and ids from another scope, are **skipped silently**, so
a shorter answer than you asked for is not an error.

## Arguments

- `document_ids` — the document ids to summarise.
- `under_path` — every document named at or under this Path-tree path, e.g.
  `/docs`.
- Give at least one of the two; with neither you get an empty list.
- `scope` — `user` (default), `agent` or `tenant`.
- `limit` — at most this many documents (default 500, at most 5000).

## Returns

`{documents: [{document_id, title, root_chunk_id, color_enabled, type?,
status?, color_scheme?}]}`. `type` and `status` are the root chunk's. When more
documents matched than `limit`, you also get `truncated: true` and a `note`.
There is no cursor: to page, list the directory with `Path op=ls` and pass each
page's ids as `document_ids`.

## Errors

- `documents_summary: invalid path segment ...` / `path must be absolute ...` —
  `under_path` is not a valid path. Fix it; see `{"op":"help","topic":"Path"}`.
- An empty `documents` list means none of the ids exist in this scope. Check
  `scope` before concluding they are gone.

## Examples

Summarise everything under `/docs`:

```json
{"op": "documents_summary", "under_path": "/docs"}
```

```json result
{"documents": [{"document_id": "b4407b522dc11495d3de371311db17f0", "title": "Launch plan", "root_chunk_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "type": "plan", "status": "draft", "color_enabled": false}]}
```

Summarise a page of ids you got from a directory listing:

```json
{"op": "documents_summary", "document_ids": ["b4407b522dc11495d3de371311db17f0", "3f2a9c1e7d6b4a5f8e0d1c2b3a4f5e6d"], "scope": "tenant"}
```
