---
name: Git/show
description: "Git op=show — one commit's details and its patch against its first parent."
---
`show` returns one commit and the changes it made, as a unified diff against
its first parent (a first commit is shown against an empty tree). It changes
nothing and works on a read-only volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `rev` — the commit (hash, branch, tag, `HEAD~1`). Default HEAD. A tag is
  followed to the commit it points at.
- `max_bytes` — cut the patch at this many bytes (default 65536, at most
  1048576).

## Returns

`{commit: {hash, author, author_email, author_date, committer,
committer_email, commit_date, message, parents}, patch, truncated}`.
`truncated` is `true` when the patch was cut at `max_bytes`; ask again with a
larger `max_bytes`, or use `diff` with `paths` to see one file. Renames show
as a deletion and an addition. A binary file, or a change too large to diff,
is named with a note instead of its content.

## Errors

- `invalid_revision` — `rev` names no commit (also: HEAD in a repository with
  no commit yet).
- `invalid_argument` — `rev` is malformed.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

The latest commit:

```json
{"op": "show", "repo": "service"}
```

A tagged release, with more room for a large patch:

```json
{"op": "show", "repo": "service", "rev": "v1.4.0", "max_bytes": 262144}
```

```json result
{"commit": {"hash": "4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d", "author": "Dana", "author_email": "dana@example.com",
  "author_date": "2026-10-02T11:00:00Z", "committer": "Dana", "committer_email": "dana@example.com",
  "commit_date": "2026-10-02T11:00:00Z", "message": "Release 1.4.0\n",
  "parents": ["0e1d2c3b4a5f6e7d8c9b0a1f2e3d4c5b6a7f8e9d"]},
 "patch": "diff --git a/VERSION b/VERSION\n...", "truncated": false}
```
