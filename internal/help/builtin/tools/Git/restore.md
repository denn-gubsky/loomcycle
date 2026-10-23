---
name: Git/restore
description: "Git op=restore — discard unstaged changes to tracked files, or with staged unstage them; untracked files are never touched."
---
`restore` puts files back:

- by default it discards the changes on disk, writing back the version that
  is staged (if nothing is staged for the file, the last committed one);
- with `staged` it unstages: the index goes back to HEAD and the files on
  disk keep your changes;
- with `staged` and `worktree` it does both: the files match HEAD again.

Untracked files are never touched (remove them with `clean`). Needs a
read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `paths` (required) — repository-relative files or directories; `["."]` is
  everything.
- `staged` — unstage: reset the index entries to HEAD.
- `worktree` — discard the changes on disk. This is the default when `staged`
  is not set; pass it with `staged` to do both.

## Returns

`{op: "restore", ok: true, head, files}` — `head` and `files` as `status`
reports them after the change.

## Errors

- `invalid_argument` — `paths` is missing, or matches no tracked file.
- `volume_refused` — the volume is read-only.

## Examples

Throw away the edits to one file:

```json
{"op": "restore", "repo": "service", "paths": ["src/retry.go"]}
```

Unstage everything but keep the edits:

```json
{"op": "restore", "repo": "service", "paths": ["."], "staged": true}
```

Make a file match the last commit again, staged and on disk:

```json
{"op": "restore", "repo": "service", "paths": ["config.yaml"], "staged": true, "worktree": true}
```
