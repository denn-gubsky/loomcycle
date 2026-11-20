---
name: Workspace/release
description: "Workspace op=release — delete one of this run's workspaces now, files and all."
---
`release` deletes the workspace and everything in it now, rather than when
your run ends. It frees one of your run's workspace slots.

## Arguments

- `name` (required) — the workspace.

## Returns

`{name, released: true}`. If its worker is away, the files are deleted when
it comes back; the workspace is gone for you either way.

## Errors

- `not_found` — this run has no workspace of that name.

## Examples

```json
{"op": "release", "name": "analysis"}
```

```json result
{"name": "analysis", "released": true}
```
