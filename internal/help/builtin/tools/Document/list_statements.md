---
name: Document/list_statements
description: "Document op=list_statements — the review queue: relations with a given status (default proposed) across the scope, newest first or least recently verified first, narrowed by document, end, evidence source, method, actor, path, confidence, creation time or judge verdict; or, with summarize, the queue's counts by group."
---
`list_statements` lists the relations awaiting review, or any other status,
across every document of the scope. Each carries its endpoints' titles and
documents, whether the runtime made it (`auto`), and up to three evidence rows,
so a reviewer can decide without opening each one. The same filters let a
maintainer pick the relations to re-check. With `summarize`, it answers how
much is waiting and where, before you page through any of it.

## Arguments

- `status` — `proposed` (default), `asserted` or `rejected`. One status per call.
- `kind` / `kinds` — only these relation kinds, each widened to the kinds the
  ontology declares beneath it. `kind_exact` — only exactly this kind, never
  widened.
- `document_id` — only relations with either end in this document.
  `from_document_id` / `to_document_id` — only relations whose FROM (or TO) end
  is in this document.
- `evidence_document_id` — only relations with an evidence row quoting a chunk
  of this document.
- `method_prefix` — only relations whose method starts with exactly this text,
  e.g. `rule:` or `extractor:cite`. Case-sensitive; `_` and `%` are ordinary
  characters. `method_exact` — only this exact method: `rule:cite` does not
  match `rule:citation`.
- `actor` — only relations written by this actor: the agent that wrote it in a
  run, else the user.
- `under_path` — only relations with either end in a document named at or under
  this Path-tree path. `from_under_path` / `to_under_path` — the same, for the
  FROM (or TO) end only.
- `from_path_prefix` — only relations whose FROM document's path prefix is
  exactly this, as `summarize` groups them: the first two segments of the
  document's first Path name in sort order (e.g. `/board/regulatory-external`).
  `from_unfiled: true` — only relations whose FROM document has no Path name.
- `min_confidence` — only relations whose writer's own confidence is at least
  this (0..1); one with no confidence is left out.
- `created_before` / `created_after` — only relations created before (or after)
  this instant, in unix nanoseconds as `created_at` reads.
- `judge_verdict` — only relations whose recorded judge verdict is exactly this
  (`accept`, `reject` or `unsure`). `has_judge_verdict` — `true` keeps only
  relations a judge recorded a verdict on, `false` only those with none.
- `include_retired` — also list relations that have expired or whose
  `invalid_at` has passed. Off by default. A relation dated to take effect
  later is listed either way.
- `summarize` — `true` returns the queue's counts instead of the relations (see
  Returns). The filters apply; `after`, `limit` and `order` are ignored.
- `order` — `created` (default): newest first. `verified`: least recently
  verified first, the never-verified ones before any.
- `limit` — page size (default 500). `after` — the `next_cursor` of the
  previous page, with the same `order`; a cursor from the other order is
  refused.
- `scope` — `user` (default), `agent` or `tenant`.

Filters combine: a relation is listed only when it passes every one given. A
filter never shows more than the plain listing would: naming a document you may
not read selects nothing, and a path filter counts only documents you may read.

## Returns

`{statements: [...]}`, each shaped like a `get_edges` row.
`truncated: true` with `next_cursor` means there is another page; the last
page carries neither.

With `summarize: true`: `{summary: [...], total}`, largest group first. Each
group has `kind`, `method`, `from_path_prefix` (empty for a document with no
Path name), `judge_verdict` (empty for none) and `count`. To page through
exactly one group, repeat the call's own filters and add the group's keys as
exact selectors: `kind_exact`, `method_exact`, then `from_path_prefix` (or
`from_unfiled: true` when it is empty), then `judge_verdict` (or
`has_judge_verdict: false` when it is empty).

Ruling is not an agent's: `accept_statement` and `reject_statement` are refused
inside a run. An operator rules on what you find here, one at a time or many at
once from the review queue; tell them which group or relations need a decision.

## Examples

What is waiting, by group:

```json
{"op": "list_statements", "scope": "tenant", "summarize": true}
```

```json
{"op": "list_statements", "status": "proposed", "limit": 50}
```

The citations a rule proposed from the external regulations, oldest checks first:

```json
{"op": "list_statements", "scope": "tenant", "status": "asserted", "method_exact": "rule:citation", "from_path_prefix": "/board/regulatory-external", "order": "verified", "limit": 100}
```

What cites a document:

```json
{"op": "list_statements", "scope": "tenant", "status": "asserted", "kind": "cites", "to_document_id": "<document id>"}
```

```json
{"op": "list_statements", "status": "asserted", "evidence_document_id": "<document id>"}
```
