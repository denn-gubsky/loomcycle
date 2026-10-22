---
name: Git/mv
description: "Git op=mv — rename or move a tracked file, on disk and in the index."
---
`mv` renames a tracked file and stages the rename. Needs a read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `from` (required) — the tracked file's repository-relative path.
- `to` (required) — the new path. It must not exist.

## Returns

`{op: "mv", ok: true, head, files}` — `head` and `files` as `status` reports
them after the change.

## Errors

- `already_exists` — something is already at `to`.
- `invalid_argument` — `from` is not a tracked file, or a path is absolute,
  has a `..` component or reaches into `.git`.
- `volume_refused` — the volume is read-only.

## Examples

```json
{"op": "mv", "repo": "service", "from": "src/retry.go", "to": "src/backoff/retry.go"}
```
