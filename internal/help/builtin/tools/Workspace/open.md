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
- `egress` — the routes the workspace may reach the network through, from
  those your grant names (Context op=self shows them). Omit for all of them;
  `[]` for none. A command reaches a route at `http://egress/<route>/…`;
  nothing else on the network. The routes are fixed when the workspace is
  opened: opening the same name again returns it with the routes it has.
- `template` — a template to start from, by name (`templates` lists them):
  your own first, then your tenant's. The workspace starts as a copy of its
  files and is then yours. Not with `toolchain`: the template names its own.

## Returns

`{name, state, engine, template?, egress?}`; `state` is `ready`; `egress`
lists the routes when it has any.

## Errors

- `not_granted` — `egress` names a route your grant does not.
- `limit` — your run (or your tenant) holds as many workspaces as allowed:
  `release` one first.
- `no_worker` — no attached worker serves you with that toolchain.
- `at_capacity` — every worker that could take it is full.
- `template_not_found` — no template of that name you may use.
- `workspace_over_quota` — the template holds more than a workspace may.

## Examples

A workspace for a data script:

```json
{"op": "open", "name": "analysis", "toolchain": "python"}
```

```json result
{"name": "analysis", "state": "ready", "engine": "host"}
```
