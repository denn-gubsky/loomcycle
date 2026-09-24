---
name: Document/get_version
description: "Document op=get_version — read the exact body a chunk had at one revision from its history."
---
`get_version` returns one past version of a chunk's body, exactly as it was
written. Take the revision number from `history`: only the revisions listed
there have a stored body. For the current text, `get_chunk` is simpler.

## Arguments

- `id` (required) — the chunk id.
- `revision` (required) — an integer from `history`.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{chunk_id, revision, body}`.

## Errors

- `get_version: missing required field: revision`.
- `get_version: no such revision N for chunk ...` — that revision changed no
  body (a title- or status-only edit), or the chunk is not in this scope. Call
  `history` and pick a listed number; guessing again is pointless.

## Examples

Read the text a chunk had at creation:

```json
{"op": "get_version", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "revision": 1}
```

```json result
{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "revision": 1, "body": "Run the migration in one step."}
```
