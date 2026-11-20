---
name: Workspace/open
description: "Workspace op=open — open a workspace for this run, or return the one of that name it already holds."
---
`open` places a new workspace on a worker that can take it and returns once
its directory exists. Opening a name your run already holds returns that
workspace unchanged.

## Arguments

- `name` (required) — your label for it in this run: 1–64 letters, digits,
  `.`, `-` or `_`, starting with a letter or digit.
- `toolchain` — `python` or `node`: the worker must have it. Omit for a
  general workspace, which goes on a worker with both.

## Returns

`{name, state, engine}`; `state` is `ready`.

## Errors

- `limit` — your run (or your tenant) holds as many workspaces as allowed:
  `release` one first.
- `no_worker` — no attached worker serves you with that toolchain.
- `at_capacity` — every worker that could take it is full.

## Examples

A workspace for a data script:

```json
{"op": "open", "name": "analysis", "toolchain": "python"}
```

```json result
{"name": "analysis", "state": "ready", "engine": "host"}
```
