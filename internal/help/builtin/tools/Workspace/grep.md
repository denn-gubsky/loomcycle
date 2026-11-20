---
name: Workspace/grep
description: "Workspace op=grep — search file contents in a workspace with a regular expression; the Grep tool's arguments without volume."
---
`grep` searches the workspace's files with an RE2 regular expression, as
the Grep tool does.

## Arguments

- `name` (required) — the workspace.
- `pattern` (required); `path`, `glob`, `output_mode`, `case_insensitive`,
  `multiline`, `head_limit`, `-A`, `-B`, `-C` as Grep takes them.

## Returns

As Grep: matching paths, lines or counts, relative to the workspace.

## Examples

```json
{"op": "grep", "name": "analysis", "pattern": "def main", "output_mode": "content"}
```
