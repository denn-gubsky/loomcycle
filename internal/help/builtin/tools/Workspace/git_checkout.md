---
name: Workspace/git_checkout
description: "Workspace op=git_checkout — switch a git workspace to a branch, or to a commit detached."
---
`git_checkout` switches the workspace to `branch`, or to the commit `rev`
names, detached. A change of yours the switch would overwrite is refused.
`git_push` still pushes only the workspace's own branch, whatever is
checked out.

## Arguments

- `name` (required) — your git workspace.
- `branch` — a local branch to switch to.
- `rev` — instead, a commit to check out detached.

## Returns

`{branch, head, detached}`.

## Errors

- `not_granted` — switching needs the workspace write grant.
- `worktree_dirty` — local changes would be overwritten.
- `invalid_argument`, `invalid_revision`.

## Examples

```json
{"op": "git_checkout", "name": "app", "branch": "experiment"}
```
