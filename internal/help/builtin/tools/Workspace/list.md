---
name: Workspace/list
description: "Workspace op=list — the workspaces this run holds, with their state."
---
`list` shows your run's workspaces, and those other runs of your user
shared with you. Other workspaces are never listed.

## Arguments

None.

## Returns

`{workspaces: [{name, state, engine, created_at, egress?, shared_with?}],
shared_with_me?: [{name, owner, access, engine}]}`. `state` is `opening`,
`ready`, `releasing` or `lost` (its worker has been away too long).
`egress` names the routes a workspace may reach the network through.
`shared_with` lists the runs you shared a workspace with (`with`, `access`).
`shared_with_me` names each workspace shared with you the way you pass it,
`<name>@<owner agent id>`.

## Examples

```json
{"op": "list"}
```

```json result
{"workspaces": [{"name": "analysis", "state": "ready", "engine": "host", "created_at": "2026-11-20T09:00:00Z"}]}
```
