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
  scope). The `quote` must be copied exactly from its source; one that does not
  occur there is refused.
- `valid_at` / `invalid_at` — when the relation began and stopped holding (unix
  nanoseconds). Leave `invalid_at` unset while it still holds.
- `confidence` — 0..1, how sure you are. It is recorded with the link and never
  hides it; only a verdict (`judge_fact`) can.
- `attributes` — the relation's qualifiers as a small JSON object.
- `scope` — `user` (default), `agent` or `tenant`. Both chunks must be in this
  scope. See `{"op":"help","topic":"scopes"}`.

## Returns

`{ok: true, id, from_id, to_id, kind, status, evidence_written}`. Linking the
same pair with the same kind again does not make a second link; it adds any new
evidence to the existing one, and asserts it if it was awaiting review.
`evidence_capped: true` means the link already holds as much evidence as it
keeps.

## Errors

- `from_id, to_id and kind are required` — supply all three.
- `confidence must be between 0 and 1`.
- `from_id: no such chunk` / `to_id: no such chunk` — that chunk is not in this
  scope. Chunk ids are not document ids: take them from `get_document`,
  `search` or `query_chunks`.
- `kind_reserved` — the kind is one the runtime writes; choose your own. An
  `about` link is accepted only from a fact; a fact gets its `about` links from
  `upsert_chunk` `about_ids`.
- `evidence_quote_not_found` — the quote is not in the cited chunk or turn;
  copy it exactly (`get_chunk` shows the body).
- `statement_rejected` — an operator rejected this relation; do not re-link it.

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
