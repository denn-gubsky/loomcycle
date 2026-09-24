---
name: Document/diff
description: "Document op=diff — a unified diff between two body revisions of one chunk."
---
`diff` compares two versions of a chunk's body and returns a standard unified
diff (lines starting `-` were removed, `+` added, with three lines of context).
Use it to see what changed in a chunk between two points. Both revision
numbers must come from `history` — revisions that changed only metadata have
no stored body.

## Arguments

- `id` (required) — the chunk id.
- `from_revision` (required) — the earlier revision.
- `to_revision` (required) — the later revision.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{chunk_id, from_revision, to_revision, diff}`. `diff` is the diff text, with
headers `--- rev N` and `+++ rev M`; it is an empty string when the two bodies
are identical.

## Errors

- `diff: from_revision and to_revision are required`.
- `diff: no such revision N for chunk ...` — pick a revision `history` lists.

## Examples

What changed between the first and the latest body:

```json
{"op": "diff", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "from_revision": 1, "to_revision": 4}
```

```json result
{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "from_revision": 1, "to_revision": 4,
 "diff": "--- rev 1\n+++ rev 4\n@@ -1 +1 @@\n-Run the migration in one step.\n+Run the migration in two steps.\n"}
```
