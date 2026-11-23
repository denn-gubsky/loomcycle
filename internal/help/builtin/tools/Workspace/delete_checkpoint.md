---
name: Workspace/delete_checkpoint
description: "Workspace op=delete_checkpoint — remove a checkpoint and give its room back to the workspace's quota."
---
`delete_checkpoint` removes one checkpoint of a workspace. Its bytes no
longer count against the workspace's quota.

## Arguments

- `name` (required) — your workspace.
- `checkpoint` (required) — the checkpoint's id.

## Returns

`{deleted, usage}`. `deleted` is false when there was no such checkpoint.

## Errors

- `not_granted` — deleting needs the `write` grant.

## Examples

```json
{"op": "delete_checkpoint", "name": "analysis", "checkpoint": "ck_3f9a1c0e5b7d2a64"}
```

```json result
{"deleted": true, "usage": {"used_bytes": 51002, "quota_bytes": 2147483648}}
```
