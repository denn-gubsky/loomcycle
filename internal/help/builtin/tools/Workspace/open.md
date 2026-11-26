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
- `lifetime` — `ephemeral` (the default: gone when your run ends) or
  `durable`: kept under your user and this name after your run ends, with
  its files. A later run of yours opens it again with
  `{"op": "open", "name": ..., "lifetime": "durable"}` and finds its files;
  it returns on the worker that holds it. Only one run holds it at a time.
  Needs your grant's `workspace.durable` and the operator's
  `workspace.durable.enabled`. An open without `lifetime` never reaches a
  durable workspace: it makes an ephemeral one.
- `root` — a folder of your user's own machine, by the name their personal
  worker offers it under. The workspace IS that folder: the user's real
  files, not a copy, and a command runs with the user's own rights there.
  Needs your grant's `workspace.roots`, and the user's personal worker
  attached. Not with `template`, `toolchain` or `lifetime: durable`.
  `checkpoint`, `checkpoints`, `restore`, `delete_checkpoint`, `fork`,
  `save_template` and `share` are refused on it (`not_on_root`); `release`
  leaves the folder as it is. Not offered over loomcycle's MCP server.

## Returns

`{name, state, engine, root?, template?, egress?}`; `state` is `ready`;
`root` names the root a workspace is on; `egress` lists the routes when it
has any.

## Errors

- `not_granted` — `egress` names a route your grant does not; or, for a
  durable workspace, your grant has no `durable`, or does not cover the
  engine or routes it was opened with.
- `durable_off` — durable workspaces are off here.
- `held` — another run of yours holds that durable workspace; the detail
  names its agent id. It is free when that run releases it or ends.
- `lost` — the durable workspace's worker is away: try later, or
  `release {name, discard: true}`.
- `limit` — your run (or your tenant) holds as many workspaces as allowed:
  `release` one first. For durable ones: you keep as many as allowed
  (`workspace.durable.max_per_user`); discard one first.
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
