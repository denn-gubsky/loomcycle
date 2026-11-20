---
name: Workspace/write
description: "Workspace op=write — create or replace a whole file in a workspace; the Write tool's arguments without volume."
---
`write` creates a file, or replaces it whole, as the Write tool does. Its
directory must exist (make one with `exec` and `mkdir -p`).

## Arguments

- `name` (required) — the workspace.
- `path` (required) — the file, relative to the workspace.
- `content` (required) — the whole new text, up to 1 MiB.

## Returns

`wrote <n> bytes to <path>`.

## Examples

```json
{"op": "write", "name": "analysis", "path": "report.py", "content": "print('rows:', 1204)\n"}
```
