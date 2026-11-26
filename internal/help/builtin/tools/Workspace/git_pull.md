---
name: Workspace/git_pull
description: "Workspace op=git_pull — fetch, then fast-forward your current branch to its origin/ twin."
---
`git_pull` is `git_fetch` followed by a fast-forward of the branch you
have checked out to `origin/<that branch>`. It never merges or rebases:
when the two have diverged it answers `non_fast_forward` and keeps the
fetched commits in `origin/*`, so you can `exec` `git merge` or `git
rebase` yourself, then push. A change of yours the fast-forward would
overwrite is refused (`worktree_dirty`).

## Arguments

- `name` (required) — the git workspace.

## Returns

`{name, op, up_to_date, updated, head}`. `head` is the commit your
branch is at afterwards.

## Errors

- `non_fast_forward` — your branch and its `origin/` twin have diverged.
- `invalid_revision` — the forge has no branch of that name (your own
  branch, before its first push).
- `worktree_dirty` — local changes would be overwritten; commit or stash
  them first.
- The errors of `git_fetch`.

## Examples

```json
{"op": "git_pull", "name": "app"}
```

```json result
{"name": "app", "op": "git_pull", "up_to_date": false, "updated": ["refs/remotes/origin/feature"], "head": "9c1e2f7a0b3d4c5e6f708192a3b4c5d6e7f80912"}
```
