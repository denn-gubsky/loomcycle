---
name: Git/pull
description: "Git op=pull — fetch a remote branch and fast-forward the current branch to it; refuses diverged history and uncommitted changes."
---
`pull` fetches one branch of a remote into its remote-tracking branch, then
fast-forwards the current branch to it, as `fetch` then `merge` would. It
never makes a merge commit and never rebases. Needs a read-write volume.

It works only when your agent's `git_remote` allows `fetch`, and only to a
host the operator lists. It refuses while any tracked file has uncommitted
changes. When it refuses, the fetched commits are kept: inspect them as
`origin/<branch>`.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `remote` — the remote's name (default `origin`).
- `branch` — the remote branch to pull (default: the current branch's name).
  Required when HEAD is detached.
- `credential` — `"$cred:<name>"` or `"$ghapp:<name>"` for a private
  repository; omit for a public one.

## Returns

`{op: "pull", up_to_date, head}` — `head` is HEAD's hash afterwards;
`up_to_date` is `true` when it did not move.

## Errors

- `non_fast_forward` — your branch and the remote's have diverged. Look with
  `log` and `diff` against `origin/<branch>`, then decide: for example
  `reset` to it, or a new branch from it.
- `worktree_dirty` — uncommitted changes, or untracked files the update would
  overwrite. Commit or `restore` first.
- `invalid_argument` — no such remote, HEAD is detached and `branch` is
  missing, or `branch` is invalid.
- `invalid_revision` — the remote has no such branch.
- `git_remote_not_allowed`, `git_network_disabled`, `host_not_allowed`,
  `credential_url_not_allowed`, `credential_unresolvable`, `auth_failed`,
  `not_found`, `source_too_large`, `upload_too_large`, `timeout`,
  `fetch_failed` — as for `clone`.

## Examples

Bring the current branch up to date:

```json
{"op": "pull", "repo": "service", "credential": "$cred:github"}
```

```json result
{"op": "pull", "up_to_date": false, "head": "5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f8e7d6c"}
```

Pull the remote's `main` into the current branch:

```json
{"op": "pull", "repo": "service", "remote": "origin", "branch": "main", "credential": "$cred:github"}
```
