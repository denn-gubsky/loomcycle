---
name: Workspace/git_branch
description: "Workspace op=git_branch — list a git workspace's local branches, or create one."
---
`git_branch` lists the workspace's local branches, or with `create` makes
a new one at `from` (default HEAD) without checking it out. Branches are
local: `git_push` pushes only the workspace's own branch.

## Arguments

- `name` (required) — your git workspace.
- `create` — the branch to create (needs the workspace write grant).
- `from` — where it starts (default HEAD).

## Returns

`{branches: [{name, hash, current, upstream?}]}`, or `{created}`.

## Errors

- `not_granted` — creating a branch needs the write grant.
- `already_exists`, `invalid_argument`, `invalid_revision`.

## Examples

```json
{"op": "git_branch", "name": "app", "create": "experiment"}
```
