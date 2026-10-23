---
name: Git/branch_list
description: "Git op=branch_list — the local branches, which one is current, and each one's upstream."
---
`branch_list` lists the repository's local branches, sorted by name. To see
the remote's branches, use `fetch` and then `log` with a `rev` such as
`origin/main`, or `ls_remote`.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).

## Returns

`{branches: [{name, hash, current, upstream?}]}`. `current` is `true` for the
branch HEAD is on (none when HEAD is detached). `upstream` is
`remote/branch` when the repository's configuration names one.

## Errors

- `not_a_repository` — no `.git` directory at `repo`.

## Examples

```json
{"op": "branch_list", "repo": "service"}
```

```json result
{"branches": [
  {"name": "fix-retry", "hash": "7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b", "current": true},
  {"name": "main", "hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "current": false, "upstream": "origin/main"}]}
```
