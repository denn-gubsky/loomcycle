---
name: Workspace/restore
description: "Workspace op=restore — put a workspace's files back to a checkpoint; stops its interpreters and its commands."
---
`restore` puts a workspace's files back as a checkpoint recorded them: files
made since are gone, files removed since are back. It first stops the
workspace's interpreters and everything running in its session (a server, a
background command); the next `run_cell` starts a clean interpreter and the
next `exec` a new session. The checkpoint stays, to restore again. Shares of
the workspace stay.

## Arguments

- `name` (required) — your workspace.
- `checkpoint` (required) — the checkpoint's id.

## Returns

`{checkpoint, kernels_stopped, session_stopped, usage}`.

## Errors

- `not_granted` — a restore needs the `write` grant.
- `checkpoint_not_found` — the workspace has no checkpoint of that id.
- `workspace_over_quota` — the checkpoint's files do not fit the quota
  beside what all the checkpoints hold. Delete a checkpoint first.

## Examples

```json
{"op": "restore", "name": "analysis", "checkpoint": "ck_3f9a1c0e5b7d2a64"}
```

```json result
{"checkpoint": "ck_3f9a1c0e5b7d2a64", "kernels_stopped": true, "session_stopped": true, "usage": {"used_bytes": 48213, "quota_bytes": 2147483648, "checkpoint_bytes": 48213}}
```
