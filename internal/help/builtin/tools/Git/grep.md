---
name: Git/grep
description: "Git op=grep — search the files of a commit for lines matching a regular expression; uncommitted changes are not searched."
---
`grep` searches the files of one commit, HEAD by default, for lines matching
a regular expression, in path order. It searches what the commit holds, not
the files on disk: an uncommitted change is not seen (commit it, or read the
file with the Read tool). It changes nothing and works on a read-only volume.

Binary files and files over 8 MiB are skipped.

## Arguments

- `volume`, `repo` — which repository (defaults: your default volume, its root).
- `pattern` (required) — an RE2 regular expression (Go syntax, no
  backreferences), at most 1024 bytes, matched against each line.
- `rev` — search the commit this revision names. Default HEAD.
- `paths` — search only these files or directories. A directory matches by
  whole path components: `"src"` covers `src/...` but not `src2`. `"."` is
  everything.
- `ignore_case` — match without regard to case.
- `invert` — list the lines that do NOT match.
- `max` — how many matching lines (default 100, at most 1000).

## Returns

`{op: "grep", matches: [{path, line, text}], truncated}`. `line` counts from
1; `text` is cut at 500 bytes. `truncated` is `true` when there were more
matches than `max`. A repository with no commit yet returns no matches.

## Errors

- `invalid_argument` — `pattern` is missing, too long or does not compile;
  or a path is absolute, has a `..` component or reaches into `.git`.
- `invalid_revision` — `rev` names nothing in this repository.
- `not_a_repository` — no `.git` directory at `repo`.

## Examples

Where a function is called, under `src`:

```json
{"op": "grep", "repo": "service", "pattern": "retryWithBackoff\\(", "paths": ["src"]}
```

```json result
{"op": "grep", "matches": [
  {"path": "src/client.go", "line": 88, "text": "\treturn retryWithBackoff(ctx, call)"}],
 "truncated": false}
```

Every TODO on the remote's main branch, in any case:

```json
{"op": "grep", "repo": "service", "pattern": "todo", "rev": "origin/main", "ignore_case": true, "max": 50}
```
