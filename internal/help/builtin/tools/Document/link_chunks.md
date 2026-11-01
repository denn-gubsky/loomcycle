---
name: Document/link_chunks
description: "Document op=link_chunks — assert a typed, directed relation from one chunk to another, optionally with the text that supports it."
---
`link_chunks` records that chunk `from_id` relates to chunk `to_id` by
`kind`: a clause that `cites` a law, a section that `defines` a term, a task
that `blocks` another. Every link is a **statement**: it has its own `id`,
records who made it and when, and can carry the evidence for it, a validity
interval and qualifiers. Direction matters: the link shows up in the target's
`backlinks`, not the source's.

Writing `[[Some Title]]` in a chunk body creates a `references` link
automatically. Use this op for any other kind, or when you have ids rather
than titles.

`link_chunks` **asserts**: the relation is live as soon as it is written, and
`graph_recall`, `backlinks` and `search` `expand` follow it. When you found the
relation in text and someone should check it first — or your run is not the
authority on it — use `propose_statement` instead. Where the tenant's confirmed
ontology declares relation kinds (`describe_ontology` lists them under
`relations`), a link that does not fit them is refused here, while the same
relation proposed is filed for review, marked.

## Arguments

- `from_id` (required) — the chunk the relation starts at.
- `to_id` (required) — the chunk it points to. It may be in another document
  of the same scope, but not in another scope.
- `kind` (required) — a short relationship name, e.g. `cites`, `defines`,
  `implements`, `blocks`. Reuse the same spelling. The kinds the runtime
  writes itself are refused: `supersedes`, `references`, `mentions`,
  `derived_from`, `supported_by`.
- `evidence` — the text the relation rests on: a list of
  `{chunk_id, quote}` (a chunk of this scope), `{session_id, event_seq, quote}`
  (a turn of a chat) or `{scope, natural_key, quote}` (a fact in another
  scope). Each item names exactly one source. The `quote` must be copied
  exactly from its source, at most 4000 characters; one that does not occur
  there is refused. `char_start` / `char_end` (characters) say which occurrence
  you mean when the quote appears more than once. A relation keeps up to 32
  evidence rows (the operator may set another limit).
- `valid_at` / `invalid_at` — when the relation began and stopped holding (unix
  nanoseconds). Leave `invalid_at` unset while it still holds.
- `confidence` — 0..1, how sure you are. It is recorded with the link and never
  hides it; only a verdict (`judge_fact`) can.
- `attributes` — the relation's qualifiers as a JSON object of at most 4096
  bytes, e.g. `{"aspect": "capital"}`. Where the tenant declares qualifiers for
  the kind, only those names (and their declared values) are accepted.
- `scope` — `user` (default), `agent` or `tenant`. Both chunks must be in this
  scope. See `{"op":"help","topic":"scopes"}`.

`link_chunks` takes no `method`: a link is the caller's own act, recorded as
`manual`. A rule or extractor names itself with `propose_statement`.

## Returns

`{ok: true, id, from_id, to_id, kind, status, evidence_written}`. Linking the
same pair with the same kind (and the same `valid_at`) again does not make a
second link; it adds any new evidence to the existing one, and asserts it if it
was awaiting review. A different `valid_at` is a second statement of the same
relation, for another period. `evidence_capped: true` means the link already
holds as much evidence as it keeps.

Each link is announced on the change feed of both ends' documents
(`documents/<document_id>/chunks`) as `{op: "link_chunks", edge_id, kind,
status, actor}`.

## Errors

- `from_id, to_id and kind are required` — supply all three.
- `confidence must be between 0 and 1`.
- `invalid_at must be after valid_at` — leave `invalid_at` unset while the
  relation holds.
- `attributes must be a JSON object of at most 4096 bytes`.
- `from_id: no such chunk` / `to_id: no such chunk` — that chunk is not in this
  scope. Chunk ids are not document ids: take them from `get_document`,
  `search` or `query_chunks`.
- `kind_reserved` — the kind is one the runtime writes; choose your own. An
  `about` link is accepted only from a fact; a fact gets its `about` links from
  `upsert_chunk` `about_ids`.
- `evidence_quote_not_found` — the quote is not in the cited chunk or turn;
  copy it exactly (`get_chunk` shows the body). `quote is longer than 4000
  characters` — quote the sentence, not the passage. `name exactly one source`
  — an evidence item gave none, or several.
- Only where the tenant's ontology declares relation kinds:
  - `kind_not_declared` — use a declared kind (the message lists the closest),
    or `propose_statement` to have it reviewed; `propose_entity` with
    `section: relations` suggests a new kind.
  - `domain_range_violation` — one end is not of a class the kind allows; type
    that chunk first, or link chunks of the declared classes.
  - `characteristic_violation` — the link breaks a rule of the kind: it relates
    a chunk to itself, the opposite direction already holds for an asymmetric
    kind, or a one-target kind already has another target.
  - `qualifier_violation` — an `attributes` name the kind does not declare, or
    a value outside its declared list.
- `statement_rejected` — an operator rejected this relation; do not re-link it.
- `attribute_reserved` — `attributes` names `judge`, `review_route` or
  `classified_by`, which only the runtime writes; leave them out.
- A permission error when your access-control rules do not let you link one of
  the two documents.

## Examples

Record that one task blocks another:

```json
{"op": "link_chunks", "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks"}
```

```json result
{"ok": true, "id": "4b1d9c2e7a3f4e5d8c6b0a1f2e3d4c5b", "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks", "status": "asserted", "evidence_written": 0}
```

A citation with the sentence that makes it:

```json
{"op": "link_chunks", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "cites",
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "as required by Article 12 of the Act"}]}
```

A dated, qualified relation in the tenant store, with the second occurrence of
the quote and a chat turn as a second source:

```json
{"op": "link_chunks", "scope": "tenant", "from_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "to_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "kind": "amends",
 "valid_at": 1767225600000000000, "confidence": 0.9, "attributes": {"mod_type": "wording"},
 "evidence": [{"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "quote": "Article 12 is amended", "char_start": 412, "char_end": 433},
              {"session_id": "s-7f3a9c", "event_seq": 14, "quote": "the March order amends Article 12"}]}
```
