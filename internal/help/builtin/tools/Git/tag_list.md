---
name: Git/tag_list
description: "Git op=tag_list — the repository's tags, each with the commit it points at."
---
`tag_list` lists the repository's tags, sorted by name.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).

## Returns

`{tags: [{name, hash, annotated, message?}]}`. `hash` is the commit the tag
points at, followed through annotated tags. `annotated` tags carry their
`message`; lightweight tags have none.

## Errors

- `not_a_repository` — no `.git` directory at `repo`.

## Examples

```json
{"op": "tag_list", "repo": "service"}
```

```json result
{"tags": [
  {"name": "v1.3.0", "hash": "0e1d2c3b4a5f6e7d8c9b0a1f2e3d4c5b6a7f8e9d", "annotated": false},
  {"name": "v1.4.0", "hash": "4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d", "annotated": true, "message": "Release 1.4.0\n"}]}
```
