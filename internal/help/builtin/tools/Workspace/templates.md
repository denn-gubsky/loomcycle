---
name: Workspace/templates
description: "Workspace op=templates — the templates you may open a workspace from."
---
`templates` lists the templates you may open a workspace from: your own
user's, then your tenant's. Open one with `open` and `template`.

## Arguments

- `all_users` — every user's templates of your tenant too, each with its
  `owner`: a tenant operator's session of the MCP server only.

## Returns

`{templates: [{template, scope, owner?, bytes, toolchains, created_at}]}`.

## Errors

- `not_granted` — listing needs the grant to open workspaces or to keep
  templates.

## Examples

```json
{"op": "templates"}
```

```json result
{"templates": [{"template": "pandas-ready", "scope": "user", "bytes": 41873, "toolchains": ["node", "python"], "created_at": "2026-11-25T09:12:03Z"}]}
```
