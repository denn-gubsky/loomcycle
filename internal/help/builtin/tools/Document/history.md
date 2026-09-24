---
name: Document/history
description: "Document op=history — list the revisions at which a chunk's BODY changed, newest first (who and when, no text)."
---
`history` lists a chunk's body versions: one entry each time its body was
written, starting with revision 1 at creation. Use it to see whether and when
a chunk's text changed, then read an old version with `get_version` or compare
two with `diff`.

It records BODY changes only. An `update_chunk` that changes just the title,
type, status or tags raises the chunk's `revision` without adding an entry
here, **so the chunk's current `revision` may not appear in this list** — use
the revision numbers this op returns, not the one from `get_chunk`.

## Arguments

- `id` (required) — the chunk id.
- `limit` — how many entries, newest first. Default 100.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{chunk_id, revisions: [{revision, created_at, actor}]}`. `created_at` is unix
nanoseconds; `actor` is the user the write ran for (empty when there was none).

## Errors

- `history: missing required field: id`.
- An unknown chunk returns `{revisions: []}` rather than an error — check the
  id and the scope.

## Examples

A chunk's last five body versions:

```json
{"op": "history", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "limit": 5}
```

```json result
{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "revisions": [
  {"revision": 4, "created_at": 1790236800000000000, "actor": "user-8812"},
  {"revision": 2, "created_at": 1790150400000000000, "actor": "user-8812"},
  {"revision": 1, "created_at": 1790064000000000000, "actor": "user-8812"}]}
```
