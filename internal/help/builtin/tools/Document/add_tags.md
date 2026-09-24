---
name: Document/add_tags
description: "Document op=add_tags — add tags to one chunk (id) or one document (document_id), keeping the tags it already has."
---
`add_tags` adds tags without touching the existing ones — the safe way to tag
something. (Passing `tags` to `create_chunk` or `update_chunk` REPLACES the
whole set instead.) It targets either a chunk or a document; **a document's
tags are separate from its root chunk's tags**, so tag the one you will later
filter on: `query_chunks` filters chunk tags, `query_documents` document tags.

## Arguments

- `tags` (required) — a non-empty array of tags. Surrounding spaces are
  trimmed and duplicates dropped. Nest with a slash: `area/billing/invoices`.
- `id` — the chunk to tag. Or:
- `document_id` — the document to tag. When you pass both, `id` wins.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

The target's full tag set after the change, sorted: `{chunk_id, tags}` for a
chunk, `{document_id, tags}` for a document. Adding a tag it already has is a
no-op.

## Errors

- `add_tags: missing required field: tags` — `tags` is empty or missing.
- `add_tags: target a chunk (id) or a document (document_id)` — pass one.
- `add_tags: no such chunk: ...` / `no such document: ...` — the target is not
  in this scope. A chunk id is not a document id; check which one you have.

## Examples

Tag a chunk:

```json
{"op": "add_tags", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "tags": ["billing/invoices", "q3"]}
```

```json result
{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "tags": ["billing/invoices", "q3", "urgent"]}
```

Tag the whole document instead:

```json
{"op": "add_tags", "document_id": "b4407b522dc11495d3de371311db17f0", "tags": ["runbook"]}
```
