---
name: Git/clean
description: "Git op=clean — remove untracked files (and with dirs, untracked directories), or list them with dry_run."
---
`clean` deletes files the repository does not track and does not ignore.
Tracked files and ignored files are never touched, and a nested repository
is left alone. Without `dirs`, only untracked files in tracked directories
are removed. Needs a read-write volume.

Deleted files cannot be recovered: list them with `dry_run` first.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `dirs` — also remove untracked directories, with everything in them.
- `dry_run` — only list what would be removed.

## Returns

`{op: "clean", dry_run, paths}` — what was (or would be) removed, sorted. A
directory is listed with a trailing `/`.

## Errors

- `volume_refused` — the volume is read-only.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

See what would go:

```json
{"op": "clean", "repo": "service", "dirs": true, "dry_run": true}
```

```json result
{"op": "clean", "dry_run": true, "paths": ["scratch.txt", "tmp/"]}
```

Then remove it:

```json
{"op": "clean", "repo": "service", "dirs": true}
```
