---
name: Document/list_statements
description: "Document op=list_statements — the review queue: relations with a given status (default proposed) across the scope, newest first."
---
`list_statements` lists the relations awaiting review, or any other status,
across every document of the scope. Each carries its endpoints' titles and
documents and up to three evidence rows, so a reviewer can decide without
opening each one.

## Arguments

- `status` — `proposed` (default), `asserted` or `rejected`.
- `kind` / `kinds` — only these relation kinds.
- `limit` — page size (default 500). `after` — the `next_cursor` of the
  previous page.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{statements: [...]}`, newest first, each shaped like a `get_edges` row. Rule
on one with `accept_statement` or `reject_statement`; a `rejected` one can be
accepted if it was rejected in error.
`truncated: true` with `next_cursor` means there is another page.

## Examples

```json
{"op": "list_statements", "status": "proposed", "limit": 50}
```
