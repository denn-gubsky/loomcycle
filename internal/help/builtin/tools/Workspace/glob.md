---
name: Workspace/glob
description: "Workspace op=glob — find files in a workspace by name; the Glob tool's arguments without volume."
---
`glob` lists the files whose path matches a pattern, newest first, as the
Glob tool does.

## Arguments

- `name` (required) — the workspace.
- `pattern` (required) — `*`, `?`, `[..]`, and `**` across directories.
- `path` — a directory to search in.

## Returns

One path per line, relative to the workspace.

## Examples

```json
{"op": "glob", "name": "analysis", "pattern": "**/*.csv"}
```
