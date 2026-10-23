---
name: Git/rm
description: "Git op=rm — delete tracked files and stage the deletion, or with cached stop tracking them but keep them on disk."
---
`rm` removes tracked files from the index and from disk, so the next commit
records their deletion. A directory path removes every tracked file under
it, and directories left empty are removed too. Needs a read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `paths` (required) — repository-relative files or directories.
- `cached` — `true` stops tracking the files but leaves them on disk (they
  become untracked).

## Returns

`{op: "rm", ok: true, head, files}` — `head` and `files` as `status` reports
them after the change.

## Errors

- `worktree_dirty` — a file has changes not yet committed (staged or not),
  so deleting it would lose them. Commit or `restore` them first, or use
  `cached`.
- `invalid_argument` — `paths` is missing, or matches no tracked file.
- `volume_refused` — the volume is read-only.

## Examples

Delete a file and stage the deletion:

```json
{"op": "rm", "repo": "service", "paths": ["docs/old-guide.md"]}
```

Stop tracking a build directory but keep its files:

```json
{"op": "rm", "repo": "service", "paths": ["build"], "cached": true}
```
