---
name: Workspace/status
description: "Workspace op=status — a workspace's state, and whether its directory is still on the worker."
---
`status` reports a workspace's state and, when it is `ready`, asks the
worker whether its directory is still there.

## Arguments

- `name` (required) — the workspace.

## Returns

`{name, state, engine, exists}`; `exists` only for a `ready` workspace.

## Errors

- `not_found` — this run has no workspace of that name.
- `worker_lost` — the worker is not reachable.

## Examples

```json
{"op": "status", "name": "analysis"}
```

```json result
{"name": "analysis", "state": "ready", "engine": "host", "exists": true}
```
