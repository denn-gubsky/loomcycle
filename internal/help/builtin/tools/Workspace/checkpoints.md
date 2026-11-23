---
name: Workspace/checkpoints
description: "Workspace op=checkpoints — list a workspace's checkpoints, newest first, and what they count against its quota."
---
`checkpoints` lists the checkpoints of one of your workspaces, newest first.

## Arguments

- `name` (required) — your workspace.

## Returns

`{checkpoints: [{id, label, created_at, bytes, files, mechanism}], usage}`.

## Errors

- `not_granted` — listing needs the `read` grant.

## Examples

```json
{"op": "checkpoints", "name": "analysis"}
```

```json result
{"checkpoints": [{"id": "ck_3f9a1c0e5b7d2a64", "label": "packages installed", "created_at": "2026-11-23T22:10:04Z", "bytes": 48213, "files": 37, "mechanism": "clone"}], "usage": {"used_bytes": 51002, "quota_bytes": 2147483648, "checkpoint_bytes": 48213}}
```
