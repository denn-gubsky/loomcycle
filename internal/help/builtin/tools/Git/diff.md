---
name: Git/diff
description: "Git op=diff — a unified diff of unstaged changes, staged changes, the worktree against a revision, or two revisions."
---
`diff` returns a unified diff. Which two sides it compares depends on the
arguments:

| Arguments | Compares |
|---|---|
| none | the staged version against the files on disk (unstaged changes) |
| `staged` | HEAD against what is staged (what `commit` would record) |
| `from` | that revision against the files on disk |
| `from` + `staged` | that revision against what is staged |
| `from` + `to` | one revision against the other |

It changes nothing and works on a read-only volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `staged` — compare with what is staged instead of the files on disk.
- `from` — the older revision (hash, branch, tag, `HEAD~3`).
- `to` — the newer revision. Needs `from`, and does not go with `staged`.
- `paths` — only these repository-relative files or directories.
- `max_bytes` — cut the patch at this many bytes (default 65536, at most
  1048576).

## Returns

`{patch, truncated}`. `patch` is empty when nothing differs. `truncated` is
`true` when it was cut at `max_bytes`; narrow it with `paths` or raise
`max_bytes`. Renames show as a deletion and an addition, and untracked files
are not included (stage them with `add` first). A binary file, or a change too
large to diff, is named with a note instead of its content. The patch is valid but not byte for
byte what the git program prints.

## Errors

- `invalid_argument` — `to` without `from`, `to` with `staged`, or a path
  that is absolute or reaches into `.git`.
- `invalid_revision` — `from` or `to` names nothing.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

What you changed but have not staged:

```json
{"op": "diff", "repo": "service"}
```

What the next commit would record:

```json
{"op": "diff", "repo": "service", "staged": true}
```

```json result
{"patch": "diff --git a/src/retry.go b/src/retry.go\n--- a/src/retry.go\n+++ b/src/retry.go\n@@ -12,7 +12,7 @@\n...", "truncated": false}
```

What changed in `src` between the main branch and a feature branch:

```json
{"op": "diff", "repo": "service", "from": "main", "to": "fix-retry", "paths": ["src"]}
```
