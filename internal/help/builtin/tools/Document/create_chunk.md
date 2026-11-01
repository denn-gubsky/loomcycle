---
name: Document/create_chunk
description: "Document op=create_chunk — add a chunk (title, Markdown body, type, fields, tags) to a document under a parent chunk, appended or at a position."
---
`create_chunk` adds one chunk to a document. It is appended as the last child of
its parent unless you give `after_id` or `position`. **Omit `parent_id` to add
a section directly under the document's title**; pass a chunk id to nest it
deeper. To write a fact that must not be duplicated, use `upsert_chunk`
instead.

## Arguments

- `document_id` (required) — the document (not a chunk id).
- `title` (required) — the chunk's heading.
- `body` — Markdown text. `[[Some title]]` becomes a link edge to the document
  or chunk with exactly that title, `[[/docs/spec]]` to the document at that
  path; a name that matches nothing stays plain text.
- `parent_id` — the parent chunk; it must exist in the same document. Omitted,
  the chunk goes directly under the document's title (its root chunk).
- `after_id` — insert right after this sibling (same parent, later siblings
  shift down). Overrides `parent_id` and `position`.
- `position` — explicit position among the siblings (0 = first). Nothing is
  shifted, so two siblings can share a position; prefer `after_id`.
- `type`, `status` — free-form labels, filterable with `query_chunks`.
- `fields` — a JSON object of structured fields. The key `principal` is
  reserved for an operator (it binds a subject to a user account); a chunk
  carrying it is refused.
- `tags` — the chunk's tags, e.g. `["area/sub", "urgent"]`.
- `valid_at`, `invalid_at` — the period the chunk's content is true, in unix
  nanoseconds: from `valid_at` (inclusive) to `invalid_at` (exclusive); leave
  an end out when it is open. Content time, not write time: when what the chunk says became true in the
  world. Omit when unknown; a guessed instant is worse than none. Use it for an
  edition, a tariff or a policy clause, so `search`, `related` and
  `query_chunks` with `as_of` return the version in force on a date. It does
  not make the chunk a fact.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

The new chunk, as `get_chunk` returns it: `{id, document_id, parent_id?,
position, title, type?, status?, revision: 1, body, fields?, tags?,
valid_at?, invalid_at?}`. Keep
`id` and `revision` if you will edit it.

## Errors

- `create_chunk: missing required field: document_id` / `... title`.
- `principal_binding_operator_only: create_chunk: ...` — drop `principal`
  from `fields`.
- `create_chunk: document "..." not found in this scope ...` — a mistyped
  `document_id`, or the document lives in another scope. Copy the id
  `create_document` returned and pass the same `scope`.
- `create_chunk: no such parent_id: <id> ...` — the parent does not exist in
  this scope. Omit `parent_id` for the top level, or use a chunk id you got
  back.
- `create_chunk: parent_id "..." belongs to document "...", not "..."` — a
  chunk cannot be parented across documents.
- `create_chunk: no such after_id chunk: <id>` / `after_id belongs to a
  different document`.
- `create_chunk: invalid_at (...) must be after valid_at (...)` — the period
  would contain no instant. Fix the dates, or omit the end you do not know.
- `an agent may only add a PROPOSED entity to the ontology ...` — only on the
  tenant ontology document; use `propose_entity`.

## Examples

Add a section under the document's title (no `parent_id` needed):

```json
{"op": "create_chunk", "document_id": "b4407b522dc11495d3de371311db17f0", "title": "Timeline", "body": "Beta on **1 March**, GA on 1 April.", "type": "section"}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "document_id": "b4407b522dc11495d3de371311db17f0", "parent_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "position": 0, "title": "Timeline", "type": "section", "revision": 1, "body": "Beta on **1 March**, GA on 1 April."}
```

Insert a task right after an existing one, with fields and tags:

```json
{"op": "create_chunk", "document_id": "b4407b522dc11495d3de371311db17f0", "after_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "title": "Write release notes", "type": "task", "status": "todo", "fields": {"owner": "maria", "estimate_days": 2}, "tags": ["release/notes"]}
```

Add one edition of a clause, in force from 1 January 2024 to 1 January 2026:

```json
{"op": "create_chunk", "document_id": "b4407b522dc11495d3de371311db17f0", "title": "Clause 7 (2024 edition)", "type": "norm.clause", "status": "in_force", "body": "Reserves are held at 4%.", "valid_at": 1704067200000000000, "invalid_at": 1767225600000000000}
```
