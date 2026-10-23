---
name: Git/merge
description: "Git op=merge — fast-forward the current branch to a revision; diverged branches are refused, never merged with a merge commit."
---
`merge` only fast-forwards: when the current branch's commit is an ancestor
of `rev`, the branch moves up to `rev` and the files follow. When `rev` is
already contained in HEAD, nothing changes. When the two have diverged it
refuses: there is no merge commit, no rebase and no conflict resolution.
Needs a read-write volume.

Like `checkout`, it refuses while any tracked file has uncommitted changes.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `rev` (required) — the revision to fast-forward to, e.g. `origin/main`.

## Returns

`{op: "merge", head}` — HEAD's hash afterwards.

## Errors

- `non_fast_forward` — HEAD and `rev` have diverged. Inspect them with `log`,
  `merge_base` and `diff`, then decide: for example `reset` to `rev`, or start
  a new branch from it with `checkout` and `create`.
- `worktree_dirty` — uncommitted changes, or untracked files the merge would
  overwrite. Commit or `restore` first.
- `invalid_revision` — `rev` names no commit.
- `volume_refused` — the volume is read-only.

## Examples

Bring the local branch up to what was just fetched:

```json
{"op": "merge", "repo": "service", "rev": "origin/main"}
```

```json result
{"op": "merge", "head": "5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c"}
```
