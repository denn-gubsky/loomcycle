---
name: Git/notes_list
description: "Git op=notes_list — the git notes attached to commits in a revision's history, newest commit first."
---
`notes_list` lists the notes git keeps in `refs/notes/commits` (the default
place `git notes` writes them) for commits reachable from a revision, newest
commit first. Commits without a note are skipped. It changes nothing and
works on a read-only volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `rev` — start from this revision. Default HEAD.
- `max` — how many notes (default 20, at most 200).

## Returns

`{op: "notes_list", notes: [{commit, note}]}`. `commit` is the full hash of
the commit the note is on; `note` is its text, cut at 4096 bytes. No notes,
or a repository with no commit yet, is an empty list.

## Errors

- `invalid_revision` — `rev` names nothing in this repository.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

```json
{"op": "notes_list", "repo": "service", "max": 5}
```

```json result
{"op": "notes_list", "notes": [
  {"commit": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "note": "Reviewed-by: Dana Ruiz\n"}]}
```

Notes on the history of a release tag:

```json
{"op": "notes_list", "repo": "service", "rev": "v1.4.0"}
```
