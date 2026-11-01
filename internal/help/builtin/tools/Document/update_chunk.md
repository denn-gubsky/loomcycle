---
name: Document/update_chunk
description: "Document op=update_chunk — change a chunk's title, type, status, body, fields, tags or validity period; requires the chunk's current revision."
---
`update_chunk` edits one chunk. **You must pass the chunk's CURRENT `revision`
— get it from `get_chunk` immediately before, never from memory or a guess.**
If someone changed the chunk in between, the update is refused rather than
silently overwriting their edit. Only the fields you include change; each
successful update adds 1 to the revision.

## Arguments

- `id` (required) — the chunk id (for the document's top chunk, its
  `root_chunk_id`).
- `revision` (required) — the chunk's current revision, an integer.
- `title`, `type`, `status` — new values. Passing `""` clears `type` or
  `status`. The root chunk's `title` is the document's name (see below).
- `body` — the new Markdown body, replacing the old one whole. Recorded in the
  chunk's history.
- `fields` — the new fields object, replacing the old one whole (not merged):
  send every field you want to keep.
- `tags` — the new tag set, replacing the old one; `[]` clears all tags.
- `valid_at`, `invalid_at` — the period the chunk's content is true, in unix
  nanoseconds (`invalid_at` exclusive). Each end you send replaces the stored
  one; `null` clears it; an end you leave out is kept. Content time, not write time: when what the chunk says became true in the
  world. Omit when unknown; a guessed instant is worse than none. Not
  recorded in the chunk's history (only body changes are).
- `scope` — `user` (default), `agent` or `tenant`.

Leave a key out to keep its value. Moving a chunk is `move_chunk`, not this.

**To rename a document**, `update_chunk` its root chunk's `title` (the root
chunk id is `root_chunk_id` from `get_document`). `get_document`,
`query_documents`, `documents_summary` and `export_md` report the new name. The
document's path does not move: to move it, use `Path op=mv`; to give it an
extra name, `set_path`. A `title` of `""` on the root chunk leaves the
document's name as it was.

## Subjects, `principal` and re-typing

A **subject** is a document whose root chunk carries a `natural_key`: the
node facts are `about`. It is made by `create_document` with `type`, `subject`
and `natural_key`, by an operator adopting a `propose_subject`, or by the
memory pass. Three rules hold for an update of one:

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

The updated chunk, as `get_chunk` returns it, with the new `revision`. Use that
revision for your next update. It may also carry `identity_key` and `warnings`
(a typed class's field check, as for `upsert_chunk`) and `not_announced` (a
subject re-type that was not announced).

## Errors

- `update_chunk: missing required field: revision (optimistic concurrency ...)`
  — call `get_chunk` and pass its `revision`.
- `update_chunk: revision conflict (you passed 2, current is 3) — re-read the chunk and retry`
  — someone else edited it. Call `get_chunk`, apply your change to the body
  you get back, and retry with the new revision. Retrying with the same
  revision always fails.
- `update_chunk: revision conflict (revision N was changed by a concurrent write) ...`
  — the same, lost by a hair. Re-read and retry.
- `update_chunk: no such chunk: <id>` — wrong id or wrong scope.
- `update_chunk: invalid_at (...) must be after valid_at (...)` — the period,
  with the ends you did not send kept as stored, would contain no instant.
- `an agent may not modify the ontology document ...` — the tenant ontology is
  operator-only.
- `principal_binding_operator_only: update_chunk: the principal field is
  reserved ...` — resend `fields` without `principal` (or with exactly the
  stored value).
- `update_chunk: "<type>" is not an entity type this tenant declares, and this
  chunk is the subject <key> ...` — re-type a subject only to a declared entity
  type.
- `identity_collision: ...` — another chunk already is the thing these identity
  fields name; update that one instead.

## Examples

Rename a document, at its root chunk's revision 2:

```json
{"op": "update_chunk", "id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "revision": 2, "title": "Launch plan v2"}
```

Change a chunk's status and body at revision 3:

```json
{"op": "update_chunk", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "revision": 3, "status": "final", "body": "Beta on **8 March**, GA on 1 April."}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "document_id": "b4407b522dc11495d3de371311db17f0", "position": 0, "title": "Timeline", "status": "final", "revision": 4, "body": "Beta on **8 March**, GA on 1 April."}
```

Replace a chunk's fields and clear its tags (every field you want kept must be
sent again):

```json
{"op": "update_chunk", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "revision": 4, "fields": {"owner": "maria", "estimate_days": 3}, "tags": []}
```

Close an edition that a newer one replaced, and mark it repealed — the status
for people, the period for `as_of`:

```json
{"op": "update_chunk", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "revision": 5, "status": "repealed", "invalid_at": 1767225600000000000}
```
