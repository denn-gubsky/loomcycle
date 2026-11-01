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
  still states it, or an operator accepted it.
- `method` — with `relation`: how the relation was found, `rule:<name>` or
  `extractor:<name>`, recorded on it in place of the memory pass's own method.
  Only an agent granted that kind of method may send it (`method_not_granted`
  otherwise); see `propose_statement`.
- `scope` — `user` (default), `agent` or `tenant`.

Every time field you omit keeps its stored value. The server sets the fact's
first-seen time itself and records who wrote it; you cannot set either.

Where the tenant's confirmed ontology gives `type` a class with typed fields,
the write is checked against them: a missing or mistyped field is reported in
`warnings` (the write still happens), and a class with identity fields records
the chunk's identity (`identity_key`), which must be unique in the scope.

## Subjects, `principal` and re-typing

A **subject** is a document whose root chunk carries a `natural_key`: the
node facts are `about`. It is made by `create_document` with `type`, `subject`
and `natural_key`, by an operator adopting a `propose_subject`, or by the
memory pass. Three rules hold for a write that lands on one:

- **`principal` is reserved.** In `fields`, on any chunk, `principal` binds a
  subject to a user account, and only an operator sets or clears it. A write
  that would set, change or clear it is refused with
  `principal_binding_operator_only` and nothing is written. Leave the key out
  of `fields` (the stored binding is kept), or send exactly the value you read.
- **Re-typing a subject** (a new `type` on its root chunk): once the tenant
  has an ontology, the new type must be one of its entity types
  (`describe_ontology` lists them); otherwise it is refused and nothing is
  written. The `natural_key` is kept: it names the subject, and its
  prefix records the kind it was first given. A re-type is announced on the
  scope's `memory/subjects` channel as `retyped` (with `old_type` and
  `new_type`), so whatever indexes relations by subject can re-check them; when
  your access-control rules hide part of the scope, it is not announced and the
  result carries `not_announced` saying why.
- **A merged subject** has status `merged` and `fields.merged_into` naming the
  surviving subject's document. An operator merged it; write new facts about the
  survivor, not about it.

## Returns

`{id, natural_key, created}` — `created: true` for a new chunk, `false` for an
update. With `source_chunk_id`, also `derived_from: {statement_id, chunk_id,
char_start, char_end}`; with `about_ids`, `about_written` and `about_skipped`;
with `relation`, `relation: {statement_id, kind, status}` or `relation_skipped`,
and `relation_conflicts`. `identity_key` and `warnings` come from the class
rules above; `not_announced` from a re-type that was not announced. Call
`get_chunk` for the full chunk and its `entity` block.

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
- `principal_binding_operator_only: upsert_chunk: the principal field is
  reserved ...` — drop `principal` from `fields`.
- `upsert_chunk: "<type>" is not an entity type this tenant declares, and this
  chunk is the subject <key> ...` — re-type a subject only to a declared entity
  type, or `propose_entity` the type first.
- `identity_collision: chunk <id> already holds the identity <key> ...` —
  update that chunk instead, or change the identity fields if this is a
  different thing. A placeholder holding it (a cited work not imported yet) is
  filled with `import_md` `reimport: true` and its `work_key`.
- `method_not_granted` — `method` is malformed or outside your agent's grant;
  omit it.

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

A fact taken from a passage of a document, about two subjects, stating the
relation between them — linked to the passage it came from, to both subjects,
and filed as a relation with the fact as its evidence:

```json
{"op": "upsert_chunk", "scope": "tenant", "document_id": "3e4f5a6b7c8d49e0a1b2c3d4e5f60718", "natural_key": "person:ada-lovelace|works_at|organization:acme-bank",
 "title": "Ada Lovelace works at Acme Bank", "body": "Ada Lovelace works at Acme Bank.", "type": "employment", "subject": "Ada Lovelace",
 "source_chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "source_quote": "Ada Lovelace, Head of Risk at Acme Bank",
 "about_ids": ["2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f", "9a8b7c6d5e4f40312a1b0c9d8e7f6a5b"], "relation": "works_at",
 "valid_at": 1735689600000000000, "confidence": 0.8}
```

```json result
{"id": "6f7a8b9c0d1e42f3a4b5c6d7e8f90a1b", "natural_key": "person:ada-lovelace|works_at|organization:acme-bank", "created": true,
 "derived_from": {"statement_id": "1a2b3c4d5e6f47a8b9c0d1e2f3a4b5c6", "chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "char_start": 210, "char_end": 249},
 "about_written": 2, "about_skipped": 0, "relation": {"statement_id": "7d8e9f0a1b2c43d4e5f6a7b8c9d0e1f2", "kind": "works_at", "status": "asserted"}}
```
