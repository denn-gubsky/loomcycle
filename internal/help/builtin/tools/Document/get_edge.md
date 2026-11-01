---
name: Document/get_edge
description: "Document op=get_edge — one relation (statement) by id, with every evidence row."
---
`get_edge` reads one relation in full: its status, how and by whom it was
made, its validity, verdict and qualifiers, and **all** of its evidence —
`get_edges` and `backlinks` show only the first three rows.

## Arguments

- `id` (required) — the statement id, from `get_edges`, `backlinks` or
  `list_statements`.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{statement: {id, from_id, to_id, kind, status, method, created_at,
from_title, from_document_id, to_title, to_document_id, evidence: [...],
evidence_count, ...}}`, plus the envelope fields `get_edges` describes when
they are set (`confidence`, `valid_at`, `invalid_at`, `attributes`, `actor`,
`verified_at`, `expired_at`, the verdict, `operator_ruled_at`). Unlike
`get_edges`, it reads a statement of any status, a rejected or retired one
too. Each evidence row has its `id` (what `maintain_statement` takes as
`evidence_id`), its `quote` and its source —
`chunk_id`, `session_id` + `event_seq`, or `scope` + `natural_key` — and
`char_start` / `char_end` where the quote sits. `stale: true` marks a quote
that no longer occurs in its source. Evidence citing a document you may not
read is left out, and `evidence_count` counts only what is shown. `confidence`
is the writer's own; a judge's finding is `verdict_confidence`.

## Errors

- `get_edge: missing required field: id`.
- `get_edge: no such statement` — check the id and the scope.

## Examples

```json
{"op": "get_edge", "id": "4b1d9c2e7a3f4e5d8c6b0a1f2e3d4c5b"}
```
