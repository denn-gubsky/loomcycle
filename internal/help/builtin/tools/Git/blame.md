---
name: Git/blame
description: "Git op=blame — for each line of one committed file, the commit, author and date that last changed it."
---
`blame` attributes each line of one file, as a commit holds it, to the commit
that last changed that line. It reads the history only: uncommitted changes
are not seen. It changes nothing and works on a read-only volume.

It follows the path only. A file that was renamed starts its history at the
rename, so lines older than that are attributed to the commit that renamed
it. In a shallow clone, lines that reach the oldest fetched commit are
attributed to it.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `paths` (required) — exactly one entry: the file, relative to the
  repository.
- `rev` — blame the file as this revision holds it. Default HEAD.
- `max` — how many lines, from the top of the file (default 500, at most
  5000).

## Returns

`{op: "blame", lines: [{line, hash, author, author_email, date, text}],
truncated}`. `line` counts from 1; `hash` is the full hash of the commit that
last changed the line; `text` is cut at 500 bytes. `truncated` is `true` when
the file has more lines than `max`.

## Errors

- `invalid_argument` — `paths` does not hold exactly one entry; there is no
  file at that path in `rev`; the file is binary; some version of it is over
  8 MiB; one change in its history touches over 5000 lines; or its history
  is too long to follow. Use `log` with `paths` and `show` instead.
- `invalid_revision` — `rev` names nothing in this repository.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

Who wrote the first 40 lines of a file:

```json
{"op": "blame", "repo": "service", "paths": ["src/retry.go"], "max": 40}
```

```json result
{"op": "blame", "lines": [
  {"line": 1, "hash": "4b2d8e1f0a9c7b6d5e4f3a2b1c0d9e8f7a6b5c4d", "author": "Dana Ruiz",
   "author_email": "dana@example.com", "date": "2026-09-30T14:02:11Z", "text": "package retry"}],
 "truncated": true}
```

The same file as the `v1.3.0` tag holds it:

```json
{"op": "blame", "repo": "service", "paths": ["src/retry.go"], "rev": "v1.3.0"}
```
