---
name: Workspace/git_diff
description: "Workspace op=git_diff — a git workspace's changes as a patch, cut at 1 MiB."
---
`git_diff` answers a patch of a git workspace's changes: the files against
the index, or with `staged` the index against HEAD, or with `rev` the
files against that commit. It is cut at 1 MiB and says so. A workspace
shared with you answers it too.

## Arguments

- `name` (required) — the git workspace, or `<name>@<agent id>`.
- `path` — only this file or directory.
- `staged` — diff the index against HEAD.
- `rev` — diff against this commit (a branch, a tag or an id).

## Returns

`{patch, truncated}`.

## Errors

- `not_a_git_workspace`, `unsupported_repository_feature` — as for
  `git_status`.
- `invalid_revision` — `rev` names no commit.

## Examples

```json
{"op": "git_diff", "name": "app", "path": "src"}
```

```json result
{"patch": "diff --git a/src/main.py b/src/main.py\n...", "truncated": false}
```
