---
name: Document/reorder_chunk
description: "Document op=reorder_chunk — move a chunk one step up or down among its siblings, without changing its parent."
---
`reorder_chunk` swaps a chunk with its neighbour above or below, under the same
parent. It never changes the parent — use `move_chunk` for that. It also
renumbers the siblings to 0, 1, 2 … so it tidies up duplicate positions left
by earlier moves. One call moves one step; call it again to move further.

## Arguments

- `id` (required) — the chunk to move.
- `direction` (required) — `up` (earlier) or `down` (later). The values `pull`
  and `push` belong to `sync` and are refused here.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{reordered: true, id, direction}` when it moved. `{reordered: false, id}` when
the chunk is already first (`up`) or last (`down`), or has no siblings — that
is a success, not an error, and repeating the call changes nothing.

## Errors

- `reorder_chunk: direction must be "up" or "down"`.
- `reorder_chunk: no such chunk: ...` — the `id` is not in this scope.
- In the tenant scope, the shared ontology document refuses edits from agents.

## Examples

Move a chunk one place earlier:

```json
{"op": "reorder_chunk", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "direction": "up"}
```

```json result
{"reordered": true, "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "direction": "up"}
```

The same chunk is now first, so a second `up` does nothing:

```json
{"op": "reorder_chunk", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "direction": "up"}
```

```json result
{"reordered": false, "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f"}
```
