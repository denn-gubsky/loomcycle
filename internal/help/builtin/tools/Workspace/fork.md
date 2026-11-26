---
name: Workspace/fork
description: "Workspace op=fork — open a second workspace of your run as a copy of one of yours, or of its checkpoint."
---
`fork` opens a new workspace of your run, named `as`, as a copy of one of
your workspaces: of its present files, or of one of its checkpoints. Use it
to try two approaches from one prepared state without either seeing the
other's changes. The fork is a workspace like any other: yours, on the same
worker, with its own session and disk quota, counted against how many
workspaces your run may hold. It starts with no checkpoints, no shares, no
interpreter running, and no record of Documents checked out into the
original (check them out again to commit from the fork). A workspace
shared with you cannot be forked. To give a sub-agent its own copy, spawn
it with `workspaces: [{fork: <name>, as?}]` (see Agent/spawn).

## Arguments

- `name` (required) — the workspace to copy.
- `as` (required) — the new workspace's name.
- `checkpoint` — a checkpoint of `name` to start from; default its present
  files.
- `branch` — for a git workspace (one opened with `repo`): the fork's own
  branch, the one its `git_push` moves. Default `loom/<as>-<id>`. A named
  branch is taken only where the operator lets a workspace adopt an
  existing branch, and never the forge's default branch or the source's
  own.

## Returns

`{name, state, engine, forked_from, checkpoint?, bytes, files, mechanism,
skipped?, rewritten?, usage}`, and for a git workspace `repo`, `branch`,
`base` and `default_branch`. `rewritten` names the files of a Python
environment whose paths were moved to the fork (on a worker whose commands
run on its own machine).

## Errors

- `not_granted` — a fork needs the grant to open workspaces.
- `exists` — your run already holds a workspace named `as`.
- `limit` — your run or your tenant holds as many workspaces as allowed.
- `at_capacity` — the worker holding `name` is full; a fork must land there.
- `workspace_over_quota` — the files do not fit a new workspace's quota.
- `checkpoint_not_found` — `name` has no checkpoint of that id.
- `invalid_branch` — a git fork's `branch` is not one it may own.

## Examples

```json
{"op": "fork", "name": "analysis", "as": "analysis-b", "checkpoint": "ck_3f9a1c0e5b7d2a64"}
```

```json result
{"name": "analysis-b", "state": "ready", "engine": "container", "forked_from": "analysis", "checkpoint": "ck_3f9a1c0e5b7d2a64", "bytes": 48213, "files": 37, "mechanism": "clone", "usage": {"used_bytes": 48213, "quota_bytes": 2147483648}}
```
