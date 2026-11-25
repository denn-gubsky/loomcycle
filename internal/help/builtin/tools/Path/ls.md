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
- `with_counts` — one-level only: add `documents` to each directory entry, and
  to a document entry that has names below it: how many documents you may read
  are named at or under it. A document named twice there counts once; a
  document entry does not count itself. Exact up to 10,000; above that
  `documents: 10000` with `documents_capped: true`. Left out — never 0 — where
  the scope's documents cannot be counted.
- `name_contains`, `type`, `status` — one-level only: list only what leads to a
  matching document. A directory is kept when a document at or under it
  matches; a document when it matches itself (or, if it has names below it,
  when one of those does). `name_contains` matches a document's Path name or
  its title, ignoring case, with `%` and `_` taken literally; a folder's own
  name is not matched. `type` includes the type's subtypes; `status` is exact.
  Each value is at most 256 characters. With `with_counts`, `documents` is the
  number that match.

Counts and filters cover only documents you may read, and a folder's
`_annotation` document is its description, not a document in it: it is
neither counted nor matched.

## Returns

`{path, entries: [{name, kind, full_path, resource_ref?, has_children?}]}`,
sorted by name (recursive: by full path). A listing cut short by `limit` also
returns `truncated: true` and a `next_cursor`.

With `with_counts` an entry may also carry `documents` and
`documents_capped`. With a filter the answer echoes it as `filters`
(`type_expanded_to` lists the subtypes a `type` took in), and a document entry
that matches itself carries `matched: true`. A counted or filtered page also
stops, with `truncated: true` and a `next_cursor`, when it has read the
metadata of 50,000 documents; the cursor resumes where it stopped, inside a
large folder too, so follow it until `truncated` is gone. Such a page can be
short, or even empty, and still have more after it.

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

The same folder with the summary of each document and sub-folder in it, to
choose which to open:

```json
{"op": "ls", "path": "/docs", "scope": "tenant", "with_summary": true, "limit": 100}
```

How many documents each folder of `/docs` holds, and which folders lead to a
draft guide:

```json
{"op": "ls", "path": "/docs", "scope": "tenant", "with_counts": true}
```

```json
{"op": "ls", "path": "/docs", "scope": "tenant", "with_counts": true, "type": "Guide", "status": "draft"}
```

```json result
{"path": "/docs", "entries": [
  {"name": "specs", "kind": "directory", "full_path": "/docs/specs", "documents": 3}],
  "filters": {"type": "Guide", "status": "draft"}}
```

Every document anywhere in this agent's tree, 100 at a time:

```json
{"op": "ls", "path": "/", "recursive": true, "kind_filter": "document", "limit": 100}
```

The next page of that listing:

```json
{"op": "ls", "path": "/", "recursive": true, "kind_filter": "document", "limit": 100, "cursor": "L2RvY3MvbGF1bmNo"}
```
