---
name: Workspace/edit
description: "Workspace op=edit — replace exact text in a file in a workspace; the Edit tool's arguments without volume."
---
`edit` replaces `old_string` with `new_string` in an existing file, as the
Edit tool does: by default `old_string` must occur exactly once.

## Arguments

- `name` (required) — the workspace.
- `path`, `old_string`, `new_string` (required); `replace_all`.

## Returns

`edited <path> (<n> replacement(s))`.

## Errors

- `failed` — the text is not in the file, or occurs more than once without
  `replace_all`.

## Examples

```json
{"op": "edit", "name": "analysis", "path": "report.py", "old_string": "1204", "new_string": "1205"}
```
