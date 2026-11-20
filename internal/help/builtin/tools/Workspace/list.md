---
name: Workspace/list
description: "Workspace op=list — the workspaces this run holds, with their state."
---
`list` shows your run's workspaces. Other runs' workspaces are never
listed.

## Arguments

None.

## Returns

`{workspaces: [{name, state, engine, created_at}]}`. `state` is `opening`,
`ready`, `releasing` or `lost` (its worker has been away too long).

## Examples

```json
{"op": "list"}
```

```json result
{"workspaces": [{"name": "analysis", "state": "ready", "engine": "host", "created_at": "2026-11-20T09:00:00Z"}]}
```
