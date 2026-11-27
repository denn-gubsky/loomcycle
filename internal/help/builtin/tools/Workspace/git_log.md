---
name: Workspace/git_log
description: "Workspace op=git_log — a git workspace's commits, newest first, at most 200."
---
`git_log` lists the commits reachable from `rev` (default HEAD), newest
first: 50 by default, at most 200. A workspace shared with you answers it
too.

## Arguments

- `name` (required) — the git workspace, or `<name>@<agent id>`.
- `rev` — where to start (a branch such as `origin/main`, a tag or an id).
- `max` — how many commits (default 50, at most 200).

## Returns

`{commits: [{hash, author, author_email, author_date, committer,
committer_email, commit_date, message, parents}]}`.

## Errors

- `not_a_git_workspace`, `unsupported_repository_feature`,
  `invalid_revision`.

## Examples

```json
{"op": "git_log", "name": "app", "max": 5}
```
