---
name: Document/unlink_chunks
description: "Document op=unlink_chunks — remove one edge, named by its from_id, to_id and kind."
---
`unlink_chunks` deletes the edge from `from_id` to `to_id` of the given
`kind`. The chunks themselves are untouched. You need all three values exactly
as the edge was created; read them from `get_edges` or `backlinks` first.

An edge that came from a `[[name]]` link in a chunk body (`auto: true` in
`get_edges`) is re-created the next time that body is written. To remove it for
good, edit the `[[...]]` out of the body with `update_chunk`.

## Arguments

- `from_id` (required) — the edge's source chunk.
- `to_id` (required) — the edge's target chunk.
- `kind` (required) — the edge kind, e.g. `blocks`.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{removed: true}` — also when no such edge existed. It does not tell you
whether anything was deleted; check `get_edges` if that matters.

## Errors

- `unlink_chunks: from_id, to_id, and kind are required` — supply all three.
  An edge with a different `kind` is a different edge.

## Examples

Remove a `blocks` edge:

```json
{"op": "unlink_chunks", "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks"}
```

```json result
{"removed": true}
```
