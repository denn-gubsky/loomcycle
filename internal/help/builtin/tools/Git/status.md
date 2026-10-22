---
name: Git/status
description: "Git op=status — the current branch and every staged, changed or untracked file in a repository."
---
`status` shows where HEAD is and which files differ from the last commit or
from what is staged. It changes nothing and works on a read-only volume.

## Arguments

- `volume` — the volume; omit for your default volume.
- `repo` — the repository's directory, relative to the volume root; omit for
  the root.

## Returns

`{head: {branch, hash, detached}, files: [{path, staging, worktree}], clean}`.

- `head.branch` is empty when HEAD is detached, and both `branch` and `hash`
  are empty in a new repository before its first commit.
- `staging` and `worktree` are one-letter codes: `" "` unchanged, `M`
  modified, `A` added, `D` deleted, `R` renamed, `C` copied, `U` in conflict,
  `?` untracked. `staging` compares the index with HEAD; `worktree` compares
  the file on disk with the index.
- `files` is sorted by path; `clean` is `true` when it is empty.

## Errors

- `not_a_repository` — the directory has no `.git` directory. Check `repo`,
  or create one with `init` or `clone`.
- `volume_refused` — the volume is not one you are bound to.

## Examples

What changed in the repository at `service`:

```json
{"op": "status", "repo": "service"}
```

```json result
{"head": {"branch": "main", "hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "detached": false},
 "files": [
  {"path": "README.md", "staging": "M", "worktree": " "},
  {"path": "notes.txt", "staging": "?", "worktree": "?"}],
 "clean": false}
```

The repository at the root of a volume named `work`:

```json
{"op": "status", "volume": "work"}
```
