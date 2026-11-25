---
name: Workspace/delete_template
description: "Workspace op=delete_template — delete a template you may delete."
---
`delete_template` deletes a template. A user template only a run of its own
user may delete; a tenant template any run of the tenant whose agent may
keep templates and holds the tenant value in its memory_scopes (over the MCP
server, a session with a tenant-operator token). Workspaces
already opened from it are not touched.

## Arguments

- `template` (required) — the template's name.
- `scope` (required) — `user` or `tenant`: which template of that name.

## Returns

`{template, scope, deleted: true}`.

## Errors

- `not_granted` — deleting needs the grant to keep templates; a tenant
  template also the tenant value in memory_scopes (over the MCP server, a
  tenant-operator token).
- `template_not_found` — no template of that name in that scope you may
  delete.

## Examples

```json
{"op": "delete_template", "template": "pandas-ready", "scope": "user"}
```

```json result
{"template": "pandas-ready", "scope": "user", "deleted": true}
```
