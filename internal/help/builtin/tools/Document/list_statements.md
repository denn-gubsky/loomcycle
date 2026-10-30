---
name: Document/list_statements
description: "Document op=list_statements — the review queue: relations with a given status (default proposed) across the scope, newest first or least recently verified first, narrowed by document, evidence source, method or path."
---
`list_statements` lists the relations awaiting review, or any other status,
across every document of the scope. Each carries its endpoints' titles and
documents, whether the runtime made it (`auto`), and up to three evidence rows,
so a reviewer can decide without opening each one. The same filters let a
maintainer pick the relations to re-check.

## Arguments

- `status` — `proposed` (default), `asserted` or `rejected`. One status per call.
- `kind` / `kinds` — only these relation kinds.
- `document_id` — only relations with either end in this document.
- `evidence_document_id` — only relations with an evidence row quoting a chunk
  of this document.
- `method_prefix` — only relations whose method starts with exactly this text,
  e.g. `rule:` or `extractor:cite`. Case-sensitive; `_` and `%` are ordinary
  characters.
- `under_path` — only relations with either end in a document named at or under
  this Path-tree path.
- `include_retired` — also list relations that have expired or are out of force
  now (their `valid_at` / `invalid_at` exclude the present). Off by default.
- `order` — `created` (default): newest first. `verified`: least recently
  verified first, the never-verified ones before any.
- `limit` — page size (default 500). `after` — the `next_cursor` of the
  previous page, with the same `order`; a cursor from the other order is
  refused.
- `scope` — `user` (default), `agent` or `tenant`.

Filters combine: a relation is listed only when it passes every one given. A
filter never shows more than the plain listing would: naming a document you may
not read selects nothing.

## Returns

`{statements: [...]}`, each shaped like a `get_edges` row. Rule on one with
`accept_statement` or `reject_statement`; a `rejected` one can be accepted if
it was rejected in error.
`truncated: true` with `next_cursor` means there is another page; the last
page carries neither.

## Examples

```json
{"op": "list_statements", "status": "proposed", "limit": 50}
```

```json
{"op": "list_statements", "status": "asserted", "method_prefix": "rule:", "order": "verified", "limit": 100}
```

```json
{"op": "list_statements", "status": "asserted", "evidence_document_id": "<document id>"}
```
