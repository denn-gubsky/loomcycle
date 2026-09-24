---
name: Document/remove_tags
description: "Document op=remove_tags — remove some tags from one chunk (id) or one document (document_id), leaving the rest."
---
`remove_tags` takes the listed tags off a chunk or a document and keeps every
other tag. Tags are matched exactly: removing `billing` does NOT remove
`billing/invoices`. To clear all tags at once, pass `tags: []` to
`update_chunk` instead.

## Arguments

- `tags` (required) — a non-empty array of the tags to remove.
- `id` — the chunk. Or:
- `document_id` — the document. When you pass both, `id` wins.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

The tags that remain, sorted: `{chunk_id, tags}` or `{document_id, tags}`.
Removing a tag the target does not have is not an error.

## Errors

- `remove_tags: missing required field: tags`.
- `remove_tags: target a chunk (id) or a document (document_id)`.
- `remove_tags: no such chunk: ...` / `no such document: ...` — the target is
  not in this scope.

## Examples

Drop one tag from a chunk:

```json
{"op": "remove_tags", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "tags": ["urgent"]}
```

```json result
{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "tags": ["billing/invoices", "q3"]}
```
