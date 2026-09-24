---
name: Document/link_chunks
description: "Document op=link_chunks — add a typed, directed edge from one chunk to another (from_id, to_id and kind are all required)."
---
`link_chunks` records a cross-reference: chunk `from_id` points at chunk
`to_id` with a relationship `kind`. Edges are how you connect chunks beyond
the parent/child hierarchy — a task that `blocks` another, a section that
`references` a spec. **All three of `from_id`, `to_id` and `kind` are
required**, and direction matters: the edge shows up in the target's
`backlinks`, not the source's.

Writing `[[Some Title]]` in a chunk body creates a `references` edge
automatically; use this op for any other kind, or when you have ids rather
than titles.

## Arguments

- `from_id` (required) — the chunk the edge starts at.
- `to_id` (required) — the chunk it points to. It may be in another document
  of the same scope, but not in another scope.
- `kind` (required) — a free-text relationship name, e.g. `references`,
  `blocks`, `depends_on`, `promotes`. Use one word and reuse the same spelling.
- `scope` — `user` (default), `agent` or `tenant`. Both chunks must be in this
  scope. See `{"op":"help","topic":"scopes"}`.

## Returns

`{ok: true, from_id, to_id, kind}`. Linking the same pair with the same kind
again is a no-op that returns the same result.

## Errors

- `link_chunks: from_id, to_id, and kind are required` — supply all three.
- `link_chunks: from_id: no such chunk: ...` / `to_id: no such chunk: ...` —
  that chunk is not in this scope. Chunk ids are not document ids: take them
  from `get_document`, `search` or `query_chunks`.

## Examples

Record that one task blocks another:

```json
{"op": "link_chunks", "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks"}
```

```json result
{"ok": true, "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks"}
```

Link chunks in the agent's own store (both must live there):

```json
{"op": "link_chunks", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "references", "scope": "agent"}
```
