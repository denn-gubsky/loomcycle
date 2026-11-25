---
name: Workspace/templates
description: "Workspace op=templates — the templates you may open a workspace from."
---
`templates` lists the templates you may open a workspace from: your own
user's, then your tenant's. Open one with `open` and `template`.

## Arguments

None.

## Returns

`{templates: [{template, scope, bytes, toolchains, created_at}]}`.

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
