---
name: Workspace/read
description: "Workspace op=read — a file's text from a workspace, whole or by lines; the Read tool's arguments without volume."
---
`read` returns a file's text, as the Read tool does.

## Arguments

- `name` (required) — the workspace.
- `path` (required) — the file, relative to the workspace.
- `offset`, `limit`, `line_numbers` — a line range, as Read takes them.

## Returns

The file's text (at most 256 KiB), or in line mode the lines and a status
line.

## Errors

- `failed` — the file does not exist, or the path leaves the workspace.
- `invalid_argument` — a line range of a binary file, or `offset` or `limit`
  below 1.

## Examples

```json
{"op": "read", "name": "analysis", "path": "out/summary.txt"}
```
