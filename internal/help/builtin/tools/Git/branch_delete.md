---
name: Git/branch_delete
description: "Git op=branch_delete — delete a local branch; an unmerged one only with force, the current one never."
---
`branch_delete` deletes a local branch and its configuration. A branch whose
commits HEAD does not contain is refused unless you pass `force`, since its
commits would no longer be on any branch. Needs a read-write volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `branch` (required) — the branch to delete.
- `force` — delete it even though HEAD does not contain its commits.

## Returns

`{op: "branch_delete", ok: true, head, files}` — `head` and `files` as
`status` reports them.

## Errors

- `invalid_argument` — no such branch, it is the current branch (`checkout`
  another first), or it is not merged into HEAD and `force` was not given.
- `volume_refused` — the volume is read-only.

## Examples

```json
{"op": "branch_delete", "repo": "service", "branch": "fix-retry"}
```

Drop an abandoned branch with commits nowhere else:

```json
{"op": "branch_delete", "repo": "service", "branch": "experiment", "force": true}
```
