---
name: Workspace/git_commit
description: "Workspace op=git_commit — commit a git workspace's changes, signed as the runtime signs them."
---
`git_commit` makes a commit on the branch checked out. Stage files first
with `paths`, or every modified and deleted tracked file with `all`. The
author is the operator's (`LOOMCYCLE_GIT_AUTHOR_NAME`) or your agent's
name, never one you choose, and a `Loomcycle-Run:` line naming your run
is added to the message. You may also commit with git in `exec`. Push with
`git_push`.

## Arguments

- `name` (required) — your git workspace.
- `message` (required) — the commit message.
- `paths` — files to stage first, relative to the workspace.
- `all` — stage every modified and deleted tracked file first.

## Returns

`{commit}` — the new commit's id.

## Errors

- `not_granted` — committing needs the workspace write grant.
- `nothing_to_commit` — nothing is staged.
- `invalid_argument` — an empty message, or a path outside the workspace.
- `unsupported_repository_feature` — as for `git_status`.

## Examples

```json
{"op": "git_commit", "name": "app", "message": "Fix the parser", "all": true}
```

```json result
{"commit": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}
```
