---
name: Git/log
description: "Git op=log — list commits newest first, from a revision or every branch, optionally only those touching one path or in a time window."
---
`log` lists commits, newest first by commit time. It changes nothing and
works on a read-only volume.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `rev` — start from this revision (hash, branch, tag, `HEAD~2`). Default HEAD.
- `all` — `true` lists commits reachable from every branch, remote branch and
  tag; `rev` is then ignored.
- `paths` — only commits that touch this path or directory. Only the FIRST
  entry is used.
- `max` — how many commits (default 20, at most 200).
- `since`, `until` — only commits in this window, as RFC 3339 times
  (`2026-10-01T00:00:00Z`).

## Returns

`{commits: [{hash, author, author_email, author_date, committer,
committer_email, commit_date, message, parents}]}`. `parents` lists full
hashes; a merge commit has two. A new repository with no commit yet returns
an empty list.

## Errors

- `invalid_revision` — `rev` names nothing in this repository.
- `invalid_argument` — `since`/`until` is not an RFC 3339 time, a path is
  absolute or reaches into `.git`, or `rev` is malformed.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

The last five commits on the current branch:

```json
{"op": "log", "repo": "service", "max": 5}
```

```json result
{"commits": [
  {"hash": "9c1e4b7a2d3f5e6a8b0c1d2e3f4a5b6c7d8e9f01", "author": "release-bot",
   "author_email": "release-bot@loomcycle.invalid", "author_date": "2026-10-21T09:14:02Z",
   "committer": "release-bot", "committer_email": "release-bot@loomcycle.invalid",
   "commit_date": "2026-10-21T09:14:02Z",
   "message": "Fix the retry loop\n\nLoomcycle-Run: run_7f3a\n",
   "parents": ["4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d"]}]}
```

Commits on `origin/main` that touched `src/api` since the first of October:

```json
{"op": "log", "repo": "service", "rev": "origin/main", "paths": ["src/api"], "since": "2026-10-01T00:00:00Z"}
```
