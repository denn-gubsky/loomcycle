---
name: Git/clean
description: "Git op=clean — remove untracked files (and with dirs, untracked directories), or list them with dry_run."
---
`clean` deletes files the repository does not track and does not ignore.
Tracked files and ignored files are never touched, and a nested repository
(any directory holding its own `.git`) is left alone, files and all.
Without `dirs`, only untracked files in tracked directories are removed;
with `dirs`, untracked files in untracked directories go too, and then each
directory left empty is removed. A directory that still holds something
(an ignored file, a nested repository) stays. Needs a read-write volume.

Deleted files cannot be recovered: list them with `dry_run` first.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `dirs` — also remove untracked files in untracked directories, and the
  directories they leave empty.
- `dry_run` — only list what would be removed.

## Returns

`{op: "clean", dry_run, paths}` — what was (or would be) removed: the files,
sorted, then the emptied directories with a trailing `/`, deepest first. A
dry run lists exactly what a real run removes.

## Errors

- `volume_refused` — the volume is read-only.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

See what would go:

```json
{"op": "clean", "repo": "service", "dirs": true, "dry_run": true}
```

```json result
{"op": "clean", "dry_run": true, "paths": ["scratch.txt", "tmp/out.log", "tmp/"]}
```

Then remove it:

```json
{"op": "clean", "repo": "service", "dirs": true}
```
