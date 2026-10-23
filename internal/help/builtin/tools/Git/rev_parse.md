---
name: Git/rev_parse
description: "Git op=rev_parse — resolve a branch, tag, short hash or expression like HEAD~2 to its full hash."
---
`rev_parse` turns a revision into the full 40-character hash it names. Use it
to pin a commit before you move branches around.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `rev` (required) — a hash (full or abbreviated), branch, tag, remote
  branch (`origin/main`), or an expression such as `HEAD~2` or `main^2`.

## Returns

`{hash}`.

## Errors

- `invalid_revision` — `rev` names nothing in this repository.
- `invalid_argument` — `rev` is missing or malformed (for example it starts
  with `-`).
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

```json
{"op": "rev_parse", "repo": "service", "rev": "HEAD~2"}
```

```json result
{"hash": "0e1d2c3b4a5f6e7d8c9b0a1f2e3d4c5b6a7f8e9d"}
```
