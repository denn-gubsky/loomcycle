---
name: Workspace/checkpoint
description: "Workspace op=checkpoint — record a workspace's files on its worker, to restore or fork from later."
---
`checkpoint` records the files of one of your workspaces, installed packages
included, and answers an id to pass to `restore` and `fork`. It records
files only: an interpreter's variables and a running command are not in it,
and both go on running. A command still writing while the checkpoint is
taken may leave a file half written in it.

A checkpoint counts its full size against the workspace's disk quota, beside
the files themselves, until you delete it. A workspace holds at most 8
checkpoints unless the operator set another bound. Checkpoints go with
their workspace.

## Arguments

- `name` (required) — your workspace.
- `label` — a note to tell it by, up to 200 printable characters.

## Returns

`{id, label, created_at, bytes, files, mechanism, skipped?, usage}`.
`bytes` is what it counts against the quota; `mechanism` is how the worker
recorded it (`clone`, `reflink` or `copy`); `skipped` names entries that are
not files, folders or links (a pipe, a socket), which are left out. `usage`
is `{used_bytes, quota_bytes, checkpoint_bytes}`.

## Errors

- `not_granted` — a checkpoint needs the `write` grant.
- `workspace_over_quota` — the files do not fit the quota a second time
  beside what the workspace and its checkpoints hold. Delete a checkpoint or
  remove files.
- `checkpoint_limit` — the workspace holds as many checkpoints as it may.

## Examples

```json
{"op": "checkpoint", "name": "analysis", "label": "packages installed"}
```

```json result
{"id": "ck_3f9a1c0e5b7d2a64", "label": "packages installed", "created_at": "2026-11-23T22:10:04Z", "bytes": 48213, "files": 37, "mechanism": "clone", "usage": {"used_bytes": 48213, "quota_bytes": 2147483648, "checkpoint_bytes": 48213}}
```
