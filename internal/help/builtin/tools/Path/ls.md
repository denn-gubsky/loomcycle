---
name: Path/ls
description: "Path op=ls — list a directory one level deep or recursively, filtered by kind, in pages."
---
`ls` lists what is under a directory. By default it shows **one level**,
including directories that exist only because something lives beneath them.

## Arguments

- `path` (required) — the directory, e.g. `/docs`. Use `/` for the root.
- `scope` — `agent` (default), `user` or `tenant`. `tenant` needs `tenant` in both the agent's `memory_scopes` and `sql_scopes`.
- `recursive` — `true` lists every descendant, flat, by full path.
- `kind_filter` — only `document`, `volume_mount`, `memory_entry` or `directory`.
- `limit` — entries per page (default 500, at most 5000).
- `cursor` — continue a truncated listing. Pass back the `next_cursor` you
  were given; never build one yourself.
- `with_summary` — tenant scope: add each entry's annotation as `summary`.
  A document gets `{level, brief, stale?}`. A folder gets `{level, brief,
  abstract, stale?}` from its `_annotation` document. A document that also
  has names below it gets its folder's as `folder_summary`. An entry with
  none has no summary, and a folder's is left out when you may not read
  every document it covers.
- `summary_chars` — with `with_summary`, the most abstract characters the page
  carries (default 2000, at most 8000).

## Returns

`{path, entries: [{name, kind, full_path, resource_ref?, has_children?}]}`,
sorted by name (recursive: by full path). A listing cut short by `limit` also
returns `truncated: true` and a `next_cursor`.

A one-level listing shows each name once, so a document that also has names
beneath it is listed as the document with `has_children: true` — list its
path to see them.

## Errors

- An empty listing is not an error: it means nothing is named under that
  directory in THIS scope. Check the scope before concluding nothing exists.
- `ls: ...` wraps a storage failure; retrying the same call is reasonable.

## Examples

What is directly under `/docs` in the tenant-shared tree:

```json
{"op": "ls", "path": "/docs", "scope": "tenant"}
```

```json result
{"path": "/docs", "entries": [
  {"name": "launch", "kind": "document", "full_path": "/docs/launch"},
  {"name": "specs", "kind": "directory", "full_path": "/docs/specs"}]}
```

Every document anywhere in this agent's tree, 100 at a time:

```json
{"op": "ls", "path": "/", "recursive": true, "kind_filter": "document", "limit": 100}
```

The next page of that listing:

```json
{"op": "ls", "path": "/", "recursive": true, "kind_filter": "document", "limit": 100, "cursor": "L2RvY3MvbGF1bmNo"}
```
