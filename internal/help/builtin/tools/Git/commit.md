---
name: Git/commit
description: "Git op=commit — record the staged changes as a new commit under the operator's identity, with a trailer naming the run."
---
`commit` records what is staged as a new commit on the current branch (or a
detached HEAD). Needs a read-write volume.

The author and committer are the identity the operator configured; without
one, your agent's name at an address under `loomcycle.invalid`. You cannot
choose them. The message gets a `Loomcycle-Run: <run id>` trailer, so a
reviewer can trace the commit to the run that made it.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `message` (required) — the commit message.
- `all` — stage changes to files already tracked (edits and deletions) first.
  New files still need `add`.
- `amend` — replace the last commit with one holding what is staged now and
  this `message`. With nothing staged it only rewords the message.
- `allow_empty` — make the commit even though nothing is staged.

## Returns

`{op: "commit", hash}` — the new commit's full hash.

## Errors

- `nothing_to_commit` — nothing is staged. Stage with `add`, or pass `all`.
- `invalid_argument` — `message` is empty, or `amend` in a repository with no
  commit yet.
- `volume_refused` — the volume is read-only.

## Examples

Stage tracked changes and commit:

```json
{"op": "commit", "repo": "service", "message": "Fix the retry loop", "all": true}
```

```json result
{"op": "commit", "hash": "7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b"}
```

Reword the last commit:

```json
{"op": "commit", "repo": "service", "message": "Fix the retry loop's backoff", "amend": true}
```
