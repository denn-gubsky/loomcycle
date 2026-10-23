---
name: Git/merge_base
description: "Git op=merge_base — the best common ancestor of two revisions."
---
`merge_base` finds where two lines of history split: the newest commit both
revisions contain. Use it to tell whether a branch can be fast-forwarded (the
merge base of HEAD and the branch is HEAD itself) or to diff only what one
side added (`diff` with `from` set to the merge base).

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `from` (required) — the first revision.
- `to` (required) — the second revision.

## Returns

`{merge_bases: [hash, ...]}`, sorted. Usually one hash; more than one when
the history has criss-cross merges; empty when the two share no history.

## Errors

- `invalid_revision` — `from` or `to` names no commit.
- `invalid_argument` — `from` or `to` is missing or malformed.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

Where the feature branch left the remote's main branch:

```json
{"op": "merge_base", "repo": "service", "from": "fix-retry", "to": "origin/main"}
```

```json result
{"merge_bases": ["4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d"]}
```
