---
name: Git/reset
description: "Git op=reset — move the current branch to a revision (soft, mixed or hard), or unstage paths; a hard reset keeps untracked files."
---
`reset` moves the current branch (or a detached HEAD) to `rev`:

- `soft` — only the branch moves; what was staged stays staged, and the
  commits undone appear as staged changes.
- `mixed` (default) — the index is reset too; your changes stay on disk,
  unstaged.
- `hard` — the index and every tracked file are reset to `rev`. Uncommitted
  changes to tracked files are lost. Untracked files are kept (unlike the
  git program), except where `rev` has a file at the same path.

With `paths` (mixed only) nothing moves: those index entries are set to their
version in `rev`, which unstages them when `rev` is HEAD. Needs a read-write
volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `mode` — `soft`, `mixed` (default) or `hard`.
- `rev` — where to move (default HEAD), e.g. `HEAD~1` or `origin/main`.
- `paths` — reset only these index entries; mixed mode only.

## Returns

`{op: "reset", ok: true, head, files}` — `head` and `files` as `status`
reports them after the change.

## Errors

- `invalid_revision` — `rev` names no commit.
- `invalid_argument` — `paths` with a soft or hard reset, or a bad path or
  revision.
- `volume_refused` — the volume is read-only.

## Examples

Undo the last commit but keep its changes staged:

```json
{"op": "reset", "repo": "service", "mode": "soft", "rev": "HEAD~1"}
```

Make the branch match the remote's exactly, discarding local commits and
changes to tracked files:

```json
{"op": "reset", "repo": "service", "mode": "hard", "rev": "origin/main"}
```

Unstage one file:

```json
{"op": "reset", "repo": "service", "paths": ["src/retry.go"]}
```
