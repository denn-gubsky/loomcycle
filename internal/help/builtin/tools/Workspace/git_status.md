---
name: Workspace/git_status
description: "Workspace op=git_status — the branch a git workspace is on and the files changed in it."
---
`git_status` reads a git workspace's repository (one opened with `repo`):
the branch checked out, its commit, and each file that differs from the
index or from HEAD, in git's one-letter codes. A workspace shared with you
answers it too.

## Arguments

- `name` (required) — the git workspace, or `<name>@<agent id>` for one
  shared with you.

## Returns

`{branch, head, detached, files: [{path, staging, worktree}]}`.

## Errors

- `not_a_git_workspace` — the workspace was not opened with `repo`.
- `unsupported_repository_feature` — git in the shell turned on something
  the typed ops cannot read (a sparse checkout, a split index, ...); the
  detail says how to undo it. `git status` in `exec` still works.

## Examples

```json
{"op": "git_status", "name": "app"}
```

```json result
{"branch": "loom/app-3f9a1c", "head": "9c1e2f7a0b3d4c5e6f708192a3b4c5d6e7f80912", "detached": false, "files": [{"path": "README.md", "staging": " ", "worktree": "M"}]}
```
