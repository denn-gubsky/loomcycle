---
name: Document/backlinks
description: "Document op=backlinks — \"what links here\": every edge pointing TO one chunk, with the linking chunk's title, type and document."
---
`backlinks` lists the chunks that link to a given chunk — both edges made with
`link_chunks` and those made by `[[name]]` links in bodies. Use it to find
everything that refers to a chunk: the facts about an entity, the tasks
blocked by a task. It follows existing links only. To find chunks that are
merely ABOUT the same thing, use `related` (by meaning) or
`unlinked_mentions` (by title text); to find chunks by structure, use
`query_chunks`.

It looks at incoming edges only. For every edge in and out of a whole
document, use `get_edges`.

## Arguments

- `id` (required) — the chunk being linked to.
- `kind` / `kinds` — only these relation kinds.
- `as_of` — only relations that held at this moment (unix nanoseconds).
- `include_proposed` — also list relations proposed for review (default false).
- `include_refuted` / `include_retired` — also list rejected or refuted / retired
  relations, which are withheld by default.
- `limit` — at most this many, default 50, capped at 200. `after` — the
  `next_cursor` of the previous page.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{backlinks: [{id, from_id, to_id, kind, auto, status, method, created_at,
from_title, from_type, from_status, from_document_id, ...}]}`, ordered by kind,
then age. Each row is a relation with the same envelope `get_edges` returns
(evidence preview, verdict, validity when present), plus `inverse_kind` when the
tenant's vocabulary names the relation as seen from this end. `auto: true`
marks a `[[name]]` link. Empty endpoint fields are omitted.

## Errors

- `backlinks: missing required field: id`.
- An unknown chunk, or one nothing links to, returns `{backlinks: []}`.

## Examples

What links to a chunk:

```json
{"op": "backlinks", "id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f"}
```

```json result
{"backlinks": [
  {"from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "kind": "blocks", "auto": false, "from_title": "Migrate the database", "from_type": "task", "from_status": "open", "from_document_id": "b4407b522dc11495d3de371311db17f0"},
  {"from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "kind": "references", "auto": true, "from_title": "Release notes", "from_document_id": "f0e1d2c3b4a5968778695a4b3c2d1e0f"}]}
```
