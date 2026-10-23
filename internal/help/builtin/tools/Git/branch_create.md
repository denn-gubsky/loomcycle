---
name: Git/branch_create
description: "Git op=branch_create — create a branch at a revision without switching to it."
---
`branch_create` makes a new local branch. HEAD and your files stay where they
are; to create a branch and switch to it in one step, use `checkout` with
`create`. Needs a read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `branch` (required) — the new branch's name, e.g. `fix-retry` (not
  `refs/heads/...`).
- `rev` — where it starts (default HEAD).

## Returns

`{op: "branch_create", ok: true, head, files}` — `head` and `files` as
`status` reports them.

## Errors

- `already_exists` — a branch with that name exists.
- `invalid_revision` — `rev` names no commit (also: HEAD before the first
  commit).
- `invalid_argument` — the name is not a valid branch name.
- `volume_refused` — the volume is read-only.

## Examples

Keep a pointer to the current commit before a risky reset:

```json
{"op": "branch_create", "repo": "service", "branch": "backup-before-reset"}
```

A branch at the remote's tip:

```json
{"op": "branch_create", "repo": "service", "branch": "upstream-main", "rev": "origin/main"}
```
