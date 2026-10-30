---
name: Document/upsert_chunk
description: "Document op=upsert_chunk — write a fact (or any idempotent chunk) keyed by natural_key: the first call creates it, later calls update the same chunk."
---
`upsert_chunk` is create-or-update keyed by `natural_key`. The first call with
a key creates a chunk and gives it fact metadata; every later call with the same
key updates **that one chunk** — in whichever document it lives — instead of
adding another. Use it for facts and for anything a repeated pass must not
duplicate. It takes no `revision`: the last writer wins. **To correct a fact
while keeping the old version, write the correction under a NEW key and then
`supersede_chunk`** — reusing the old key overwrites it in place.

## Arguments

- `natural_key` (required) — the stable identity, unique in the scope. Use a
  derived form: `person:ada-lovelace` for a subject,
  `subject|predicate|object` for a fact. It may be omitted when `type` names a
  class whose identity fields the tenant ontology declares: the key is then
  built from those fields.
- `document_id`, `title` — required when the key is new (the chunk is created
  like `create_chunk`); ignored for placement on an update.
- `parent_id`, `after_id`, `position` — placement, on create only.
- `body`, `fields`, `status`, `title`, `type` — on an update only the ones you
  pass change; an empty string leaves a value as it was.
- `tags` — replaces the chunk's whole tag set when present; `[]` clears it.
- `type` + `subject` — pass both or neither. The pair marks this as an
  assertion about `subject` of kind `type`, and the type must be one the
  tenant ontology declares.
- `source_quote` — the exact text you derived the fact from, copied verbatim.
  A judge checks the claim against it. Kept when a later upsert omits it.
- `valid_at` — when it became true in the world (unix nanos). Omit when
  unknown; never guess.
- `invalid_at` — when it stopped being true in the world. A future value keeps
  it current until then; a past value hides it from default reads.
- `observed_at` — when it was said or written.
- `class` — `derived` (default) or `evidential` (source material, exempt from
  age-based pruning).
- `confidence` — 0 to 1. Below 0.25 the fact is withheld from fact reads.
- `from_pending` — the id of a pending memory item you processed; the server
  records where the fact came from.
- `source_session_id` — rarely used: the id of the chat session the fact was
  distilled from, when it did not come off a pending item (`from_pending`
  supplies this itself and wins). Kept when a later upsert omits it.
- `source_chunk_id` — the chunk of this scope (a passage of a document) the
  fact was taken from. The fact is linked to it (`derived_from`) with your
  `source_quote` as evidence, so it can be checked against the passage and
  recall can point back to it. The quote must occur in that chunk and be at
  most 4000 characters, and you must be allowed to read and link that
  chunk's document. A fact has one source: passing another replaces it.
- `char_start` / `char_end` — where the quote sits in the source chunk's text
  (characters). With no `source_quote`, they select it.
- `about_ids` — the subject nodes (entity chunks) the fact is about. The fact
  is linked to each (`about`); one that is not a chunk of this scope, or whose
  document you may not read and link, is skipped and counted in
  `about_skipped`. The list is the whole set: a subject an earlier upsert named
  and this one does not is no longer linked.
- `relation` — with two or more `about_ids`: the kind of relation the fact
  states between the first two you list (e.g. `works_at`); when either of those
  two is skipped, so is the relation. It is recorded as a relation whose
  evidence is the fact; a kind the tenant has not declared is recorded for
  review. Another live relation you can read, of the same kind from the same
  subject to a different one, is reported as `relation_conflicts`, never
  removed. An upsert with `about_ids` replaces the relation the fact stated
  before, and retiring or deleting the fact ends it — unless another fact
  still states it.
- `scope` — `user` (default), `agent` or `tenant`.

Every time field you omit keeps its stored value. The server sets the fact's
first-seen time itself and records who wrote it; you cannot set either.

## Returns

`{id, natural_key, created}` — `created: true` for a new chunk, `false` for an
update. With `source_chunk_id`, also `derived_from: {statement_id, chunk_id,
char_start, char_end}`; with `about_ids`, `about_written` and `about_skipped`;
with `relation`, `relation: {statement_id, kind, status}` or `relation_skipped`,
and `relation_conflicts`. Call `get_chunk` for the full chunk and its `entity` block.

## Errors

- `upsert_chunk: missing required field: natural_key ...`.
- `upsert_chunk: unknown class "..." (want derived or evidential)`.
- `create_chunk: missing required field: document_id` / `... title` — the key
  is new, so this call creates; add them.
- `upsert_chunk: "<type>" is not an entity type this tenant declares ... Declared: ...`
  — use one of the listed types, drop the `type`/`subject` pair, or suggest the
  type with `propose_entity`.
- `create_chunk: no such parent_id: ...` — see `create_chunk`.
- `upsert_chunk: source_chunk_id: no such chunk: ...` — also what a chunk you
  may not read or link answers; take the fact from a passage you can link.
- `upsert_chunk: source_quote is longer than 4000 characters` — quote the
  sentence the fact rests on, not the passage.

## Examples

Record a fact with its source quote and when it became true:

```json
{"op": "upsert_chunk", "document_id": "b4407b522dc11495d3de371311db17f0", "natural_key": "person:ada|lives_in|cluj-napoca", "title": "Ada lives in Cluj-Napoca", "body": "Ada lives in Cluj-Napoca.", "type": "residence", "subject": "Ada", "source_quote": "I moved to Cluj-Napoca in May.", "valid_at": 1746057600000000000, "observed_at": 1772323200000000000}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "natural_key": "person:ada|lives_in|cluj-napoca", "created": true}
```

Record that a still-true fact has a known end date (same key, so the same chunk
updates):

```json
{"op": "upsert_chunk", "natural_key": "person:ada|works_at|acme", "invalid_at": 1798761600000000000}
```
