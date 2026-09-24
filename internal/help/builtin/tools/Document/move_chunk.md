---
name: Document/move_chunk
description: "Document op=move_chunk — give a chunk a new parent (and position) inside the same document; its whole subtree moves with it."
---
`move_chunk` re-parents a chunk within its document. The chunk keeps its id,
body, tags and edges, and everything beneath it moves along. Use it to
restructure a document; use `reorder_chunk` to shift a chunk up or down among
its current siblings. **To put a chunk directly under the document's top
heading, pass the document's `root_chunk_id` as `new_parent_id`** — leaving
`new_parent_id` empty does something else (see below).

## Arguments

- `id` (required) — the chunk to move.
- `new_parent_id` — the chunk that becomes its parent. It must exist and be in
  the SAME document. Omitted or empty, the chunk gets NO parent: it becomes a
  second top-level chunk beside the document root, and `export_md` renders it
  as a separate `#` heading. That is rarely what you want.
- `position` — its place among the new siblings, 0-based (default 0). Other
  siblings are not shifted, so two chunks can end up sharing a position; call
  `reorder_chunk` afterwards if the order matters.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{ok: true, id, new_parent_id, position}`.

## Errors

- `move_chunk: no such chunk: ...` — the `id` is not in this scope. Find the
  chunk with `search` or `query_chunks`; do not guess ids.
- `move_chunk: no such new_parent_id: ...` — the parent does not exist.
- `move_chunk: new_parent_id ... belongs to document ...` — a chunk cannot move
  to another document. To copy content across, read it and `create_chunk` in
  the other document.
- `move_chunk: cannot move a chunk under itself` /
  `cannot move a chunk into its own subtree` — pick a parent outside the
  chunk's own branch. Retrying the same call is pointless.
- In the tenant scope, the shared ontology document refuses edits from agents;
  file a proposal instead.

## Examples

Move a section under another section of the same document:

```json
{"op": "move_chunk", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "new_parent_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "position": 2}
```

```json result
{"ok": true, "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "new_parent_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "position": 2}
```

Lift a nested chunk back up to be a direct child of the document root (the
root's id comes from `get_document`):

```json
{"op": "move_chunk", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "new_parent_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "scope": "agent"}
```
