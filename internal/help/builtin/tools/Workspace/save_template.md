---
name: Workspace/save_template
description: "Workspace op=save_template — keep a checkpoint of your workspace as a template to open new workspaces from."
---
`save_template` keeps a checkpoint of one of your workspaces as a
**template**: a named starting state, its files and installed packages
already in place. The runtime keeps it after your run ends; `open` with
`template` starts a new workspace as a copy of it, on any worker that has
its toolchains.

- `scope: user` — open to your own runs (runs of your user).
- `scope: tenant` — open to every user of your tenant. Needs the tenant
  value in your agent's memory_scopes (over the MCP server, a
  tenant-operator token).

A template is never overwritten: save under another name, or delete the
old one first. It holds files a model wrote; it never leaves your tenant.

## Arguments

- `name` (required) — your workspace (not one shared with you).
- `checkpoint` (required) — the checkpoint to keep (`checkpoints` lists them).
- `as` (required) — the template's name: 1–64 letters, digits, `.`, `-` or
  `_`, starting with a letter or digit.
- `scope` (required) — `user` or `tenant`.

## Returns

`{template, scope, bytes, toolchains, created_at}`.

## Errors

- `not_granted` — saving needs the grant to keep templates; a tenant
  template also the tenant value in memory_scopes (over the MCP server, a
  tenant-operator token).
- `exists` — a template of that name is already in that scope.
- `template_limit` — you hold as many user templates as allowed (5 unless
  the operator says otherwise), or your tenant holds as many templates as
  allowed (20): delete one first.
- `template_too_large` — the template passes the size allowed (1 GiB unless
  the operator says otherwise).
- `checkpoint_not_found` — the workspace has no checkpoint of that id.

## Examples

```json
{"op": "save_template", "name": "analysis", "checkpoint": "ck_3f9a1c0e5b7d2a64", "as": "pandas-ready", "scope": "user"}
```

```json result
{"template": "pandas-ready", "scope": "user", "bytes": 41873, "toolchains": ["node", "python"], "created_at": "2026-11-25T09:12:03Z"}
```
