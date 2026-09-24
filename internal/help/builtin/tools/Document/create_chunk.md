---
name: Document/create_chunk
description: "Document op=create_chunk — add a chunk (title, Markdown body, type, fields, tags) to a document under a parent chunk, appended or at a position."
---
`create_chunk` adds one chunk to a document. It is appended as the last child of
its parent unless you give `after_id` or `position`. **Pass the document's
`root_chunk_id` as `parent_id` to put a chunk under the title**: with no
`parent_id` the chunk has no parent at all and sits at the top level beside the
root, which renders as a second top-level heading. To write a fact that must not
be duplicated, use `upsert_chunk` instead.

## Arguments

- `document_id` (required) — the document (not a chunk id).
- `title` (required) — the chunk's heading.
- `body` — Markdown text. `[[Some title]]` becomes a link edge to the document
  or chunk with exactly that title, `[[/docs/spec]]` to the document at that
  path; a name that matches nothing stays plain text.
- `parent_id` — the parent chunk; it must exist in the same document.
- `after_id` — insert right after this sibling (same parent, later siblings
  shift down). Overrides `parent_id` and `position`.
- `position` — explicit position among the siblings (0 = first). Nothing is
  shifted, so two siblings can share a position; prefer `after_id`.
- `type`, `status` — free-form labels, filterable with `query_chunks`.
- `fields` — a JSON object of structured fields.
- `tags` — the chunk's tags, e.g. `["area/sub", "urgent"]`.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

The new chunk, as `get_chunk` returns it: `{id, document_id, parent_id?,
position, title, type?, status?, revision: 1, body, fields?, tags?}`. Keep
`id` and `revision` if you will edit it.

## Errors

- `create_chunk: missing required field: document_id` / `... title`.
- `create_chunk: no such parent_id: <id> ...` — the parent does not exist in
  this scope. Use the document's `root_chunk_id`, or a chunk id you got back.
- `create_chunk: parent_id "..." belongs to document "...", not "..."` — a
  chunk cannot be parented across documents.
- `create_chunk: no such after_id chunk: <id>` / `after_id belongs to a
  different document`.
- `an agent may only add a PROPOSED entity to the ontology ...` — only on the
  tenant ontology document; use `propose_entity`.

The tool does not check that `document_id` exists. A mistyped one creates a
chunk no document shows, so copy the id exactly.

## Examples

Add a section under the document's title:

```json
{"op": "create_chunk", "document_id": "b4407b522dc11495d3de371311db17f0", "parent_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "title": "Timeline", "body": "Beta on **1 March**, GA on 1 April.", "type": "section"}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "document_id": "b4407b522dc11495d3de371311db17f0", "parent_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "position": 0, "title": "Timeline", "type": "section", "revision": 1, "body": "Beta on **1 March**, GA on 1 April."}
```

Insert a task right after an existing one, with fields and tags:

```json
{"op": "create_chunk", "document_id": "b4407b522dc11495d3de371311db17f0", "after_id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "title": "Write release notes", "type": "task", "status": "todo", "fields": {"owner": "maria", "estimate_days": 2}, "tags": ["release/notes"]}
```
