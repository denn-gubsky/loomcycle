---
name: Workspace/unexpose
description: "Workspace op=unexpose — withdraw a page you exposed."
---
`unexpose` withdraws the page of a port you exposed: its address answers
"no such page" from then on. The server keeps running; `stop` ends it.

## Arguments

- `name` (required) — the workspace.
- `port` (required) — the port you exposed.

## Returns

`{name, port, closed}`. `closed` is false when the port was not exposed.

## Errors

- `not_granted` — this agent may not expose ports.

## Examples

```json
{"op": "unexpose", "name": "site", "port": 8000}
```
