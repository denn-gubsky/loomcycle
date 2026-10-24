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
- `under_path` / `under_paths` — only chunks of documents named at or under
  this path (any of these paths).
- `exclude_under_path` / `exclude_under_paths` — never chunks of documents named
  at or under these paths.
- `document_id` / `document_ids` — only these documents.
- `exclude_document_ids` — never these documents.
- `type` / `types` — only chunks of these types, each including its subtypes.
- `status` / `statuses` — only chunks with exactly one of these statuses.
- `tag` — only chunks carrying exactly this tag.
- `tag_prefix` — only chunks whose tag is this one or nested under it
  (`prefix/…`).
- `as_of` — only chunks whose stated validity contains this moment (unix
  nanoseconds); a chunk with no stated validity always matches.

## Aiming the neighbours

The filter arguments above narrow which chunks are compared BEFORE anything is
ranked. That matters: only the closest few dozen candidates are kept, so in a
scope that holds several kinds of document — board files, norms,
decisions, your own earlier reports — the candidates can all come from the
wrong kind. A filter makes them come from the right one. Filter whenever the
scope holds more than one kind of document; it costs one structural lookup.

Different kinds of filter combine with AND; the values of one list with OR. A
singular argument and its plural twin are one filter (`under_path: "/a"` plus
`under_paths: ["/b"]` means either path). Paths resolve in this scope's own
tree. An unknown path, type, status or tag is not an error: it matches nothing,
and the response says so. A malformed path is.

With a filter, the response also carries `filter`, what it resolved to:

```text
filter: {
  applied: {under_paths, exclude_under_paths, document_ids, exclude_document_ids,
            types, statuses, tag, tag_prefix, as_of},   (only what you passed)
  matched_documents: 14,     documents with at least one matching chunk
  matched_chunks: 312,       chunks that could be returned
  type_expanded_to: [...],   present when a type was widened to its subtypes
  inverted: false            true when the filter was sent as "everything except"
}
```

When access-control rules keep you from reading part of the scope, the filter
covers only what you may read, and so do the counts.

`matched_chunks: 0` means the filter itself matched nothing — check the path,
type or status — as opposed to an empty result with matches, which means nothing
in that part of the tree is relevant. A call with no filter has no `filter` key.

## Returns

`{related: [{chunk_id, score, title, type, document_id}]}`, closest first,
plus `filter` when you passed one.
A chunk whose body is empty — a section heading with only children — or is a
mermaid or image form has nothing to compare, and returns `{related: []}`.

## Errors

- `related: missing required field: id`.
- `related: requires a configured embedder / vector memory` — this server
  cannot compare meaning; retrying is pointless. Use `backlinks` or
  `unlinked_mentions` instead.
- `related: body: ...` / `related: embed: ...` — a storage or embedding
  failure; a retry may work.
- `related: filter_too_wide: ...` — as for `search`: narrow the filter.

## Examples

Chunks similar to one task:

```json
{"op": "related", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "limit": 5}
```

```json result
{"related": [
  {"chunk_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "score": 0.78, "title": "Rollback plan", "document_id": "b4407b522dc11495d3de371311db17f0"}]}
```

Earlier decisions like the passage under review, never from the passport itself:

```json
{"op": "related", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "scope": "tenant", "under_path": "/qg/decisions", "status": "active", "exclude_document_ids": ["b4407b522dc11495d3de371311db17f0"]}
```
