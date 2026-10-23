---
name: Git/add
description: "Git op=add — stage files for the next commit, including deletions; . stages everything."
---
`add` copies files from disk into the index, so the next `commit` records
them. A tracked file you deleted is staged as a deletion. Files the
repository ignores are skipped. Needs a read-write volume.

To stage only changes to files already tracked, skip `add` and pass `all` to
`commit`.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `paths` — repository-relative files or directories to stage. `["."]`, or
  omitting `paths`, stages everything, new files included.

## Returns

`{op: "add", ok: true, head, files}` — `head` and `files` as `status`
reports them after the change.

## Errors

- `invalid_argument` — a path matches no file, is absolute, has a `..`
  component or reaches into `.git`.
- `volume_refused` — the volume is read-only.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

Stage two files:

```json
{"op": "add", "repo": "service", "paths": ["src/retry.go", "src/retry_test.go"]}
```

```json result
{"op": "add", "ok": true, "head": {"branch": "main", "hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "detached": false},
 "files": [{"path": "src/retry.go", "staging": "M", "worktree": " "},
           {"path": "src/retry_test.go", "staging": "A", "worktree": " "}]}
```

Stage everything, new files included:

```json
{"op": "add", "repo": "service", "paths": ["."]}
```
