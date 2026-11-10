---
name: Document/list_annotations
description: "Document op=list_annotations — page through a scope's annotations, or count them by level and staleness."
---
Lists stored annotations, ordered by chunk, each marked `stale` or current.
With `summarize: true` it returns counts instead: per level, how many there
are, how many are stale and how many an operator wrote by hand. A walk uses
the counts to see what is left to do before it spends a model call.

Annotations of documents the caller may not read are left out.

## Arguments

- `level` — `branch`, `document` or `path`.
- `document_id` — one document's annotations.
- `under_path` — the annotations of documents under a Path-tree folder.
- `stale` — only stale ones.
- `manual` — only an operator's.
- `written_before` — only those written before this RFC 3339 time.
- `summarize` — counts instead of rows.
- `limit` — rows per page (default 50, at most 500).
- `after` — the `next_cursor` of the previous page.
- `scope` — `tenant`.

## Returns

`{"annotations": [record, …], "next_cursor"?}`, or with `summarize`
`{"counts": {"<level>": {"total", "stale", "manual"}}, "stale_checked"?}`.
`stale_checked` appears when the scope holds more annotations than one call
checks, and says how many were.

## Examples

```json
{"op": "list_annotations", "scope": "tenant", "summarize": true}
```

```json result
{"counts": {"branch": {"total": 1840, "stale": 12, "manual": 3}, "document": {"total": 319, "stale": 4, "manual": 0}}}
```
