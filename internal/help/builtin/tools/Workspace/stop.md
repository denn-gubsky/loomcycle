---
name: Workspace/stop
description: "Workspace op=stop — end a server started with Workspace op=start, and everything it started."
---
`stop` ends one of the workspace's servers and every process it started.

## Arguments

- `name` (required) — the workspace.
- `server` (required) — the server's id, as `start` returned it.

## Returns

`{server, stopped}`. `stopped` is false when the server had already ended
by itself; its output is still readable with `logs`.

## Errors

- `not_granted` — needs the `exec` grant.
- `server_not_found` — no server of that id; `logs` lists them.

## Examples

```json
{"op": "stop", "name": "site", "server": "srv-1"}
```

```json result
{"server": "srv-1", "stopped": true}
```
