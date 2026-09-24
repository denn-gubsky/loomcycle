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
  `subject|predicate|object` for a fact.
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
- `scope` — `user` (default), `agent` or `tenant`.

Every time field you omit keeps its stored value. The server sets the fact's
first-seen time itself and records who wrote it; you cannot set either.

## Returns

`{id, natural_key, created}` — `created: true` for a new chunk, `false` for an
update. Call `get_chunk` for the full chunk and its `entity` block.

## Errors

- `upsert_chunk: missing required field: natural_key ...`.
- `upsert_chunk: unknown class "..." (want derived or evidential)`.
- `create_chunk: missing required field: document_id` / `... title` — the key
  is new, so this call creates; add them.
- `upsert_chunk: "<type>" is not an entity type this tenant declares ... Declared: ...`
  — use one of the listed types, drop the `type`/`subject` pair, or suggest the
  type with `propose_entity`.
- `create_chunk: no such parent_id: ...` — see `create_chunk`.

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
