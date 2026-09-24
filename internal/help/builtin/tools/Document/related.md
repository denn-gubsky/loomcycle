---
name: Document/related
description: "Document op=related — the chunks whose bodies are closest in meaning to one chunk's body, ranked by score."
---
`related` takes a chunk you already have and finds other chunks, in any
document of the same scope, whose bodies say similar things. Use it to
discover connections nobody has linked yet. It is `search` with a chunk's body
as the query: use `search` when you have words, `related` when you have a
chunk, and `backlinks` when you want the chunks that already link to it.

## Arguments

- `id` (required) — the chunk to find neighbours for.
- `limit` — at most this many, default 10, capped at 50. The chunk itself is
  never included.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{related: [{chunk_id, score, title, type, document_id}]}`, closest first.
A chunk whose body is empty — a section heading with only children — or is a
mermaid or image form has nothing to compare, and returns `{related: []}`.

## Errors

- `related: missing required field: id`.
- `related: requires a configured embedder / vector memory` — this server
  cannot compare meaning; retrying is pointless. Use `backlinks` or
  `unlinked_mentions` instead.
- `related: body: ...` / `related: embed: ...` — a storage or embedding
  failure; a retry may work.

## Examples

Chunks similar to one task:

```json
{"op": "related", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "limit": 5}
```

```json result
{"related": [
  {"chunk_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "score": 0.78, "title": "Rollback plan", "document_id": "b4407b522dc11495d3de371311db17f0"}]}
```
