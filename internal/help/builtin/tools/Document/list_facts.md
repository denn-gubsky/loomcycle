---
name: Document/list_facts
description: "Document op=list_facts — browse the scope's facts newest first, metadata only, filtered by subject, type, class, document or a past moment."
---
`list_facts` lists the chunks that carry fact metadata, newest first, with each
fact's `entity` block but **no bodies** (call `get_chunk` for a body). Use it to
answer "what do we know about X": pass the subject's chunk id as `about`, which
returns the facts filed under that subject AND the facts elsewhere that point at
it. **`about` takes a chunk id — a subject document's `root_chunk_id` — not a
name and not a `document_id`.**

## Arguments

- `about` — the subject's entity chunk id (from `get_document` on the subject's
  document, or from a fact's edges).
- `across_scopes` — with `about`: also find the same subject (by its natural
  key) in your other readable scopes. Refused without `about`. Never reaches
  another user's store.
- `type` — only facts of this type, including its subtypes.
- `class` — `derived` or `evidential`.
- `claims_only` — `true` drops the subject name nodes and keeps only the
  claims.
- `document_id` — only facts filed in this document.
- `as_of` — unix nanos: facts true at that moment (see `graph_recall`).
- `include_retired` — `true` also returns superseded and ended facts.
- `include_refuted` — `true` also returns facts judged `unsupported`.
- `limit` — default 50, at most 200.
- `scope` — `user` (default), `agent` or `tenant`. One scope per call.

## Returns

`{facts: [...], count, truncated}`. Each fact is `{id, document_id, parent_id?,
position, title, type?, status?, revision, entity}`; `entity` is the same block
`get_chunk` returns (times, natural key, class, confidence, source quote,
verdict), except that `observed_at` is not included here. `type_expanded_to`
lists the types searched when `type` pulled in subtypes. With `across_scopes`,
`across_scopes: [{scope, document_id, entity_chunk_id, facts, fact_rows,
truncated?}]` holds what the other scopes know. `truncated: true` means more
facts matched than `limit`.

## Errors

- `list_facts: class must be one of: derived, evidential`.
- `list_facts: no such chunk: <id> (about takes a SUBJECT's entity chunk id ...)`
  — you passed a name, a document id, or an id from another scope.
- `list_facts: across_scopes needs about ...` — add `about`, or drop
  `across_scopes`.
- An empty `facts` list is an answer: nothing current matches in this scope.
  Try `include_retired`, or the other scope.

## Examples

Everything currently known about one subject, claims only:

```json
{"op": "list_facts", "about": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "claims_only": true}
```

```json result
{"facts": [{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "document_id": "b4407b522dc11495d3de371311db17f0", "position": 2, "title": "Ada lives in Cluj-Napoca", "type": "residence", "revision": 1,
  "entity": {"retired": false, "valid_at": 1746057600000000000, "natural_key": "person:ada|lives_in|cluj-napoca", "class": "derived", "source_quote": "I moved to Cluj-Napoca in May.", "subject": "Ada"}}],
 "count": 1, "truncated": false}
```

The same subject across the user, agent and tenant stores:

```json
{"op": "list_facts", "about": "2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "across_scopes": true}
```

Every event-type fact in the tenant store that was true on 1 March, including
ones corrected since:

```json
{"op": "list_facts", "scope": "tenant", "type": "event", "as_of": 1772323200000000000, "limit": 100}
```
