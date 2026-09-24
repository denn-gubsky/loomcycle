---
name: Document/get_chunk
description: "Document op=get_chunk — one chunk with its Markdown body, fields, tags, current revision and, for a fact, its time and provenance block."
---
`get_chunk` reads one chunk in full: its body, fields, tags, and its current
`revision`, which `update_chunk` requires. For a fact it also returns the
`entity` block (timestamps, natural key, source quote, verdict). Always call it
right before `update_chunk` so the revision you pass is current.

## Arguments

- `id` (required) — the **chunk** id. For a document's top chunk, use the
  `root_chunk_id` from `get_document`; a `document_id` here is "no such chunk".
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{id, document_id, position, title, revision, body}` plus, when set:
`parent_id`, `type`, `status`, `fields`, `tags`, and `asset: {media_type,
size}` for an image chunk (the bytes are served separately).

A fact also carries `entity`: `retired` (always present; true once a
`supersede_chunk` retired it), and when set `valid_at`, `invalid_at`,
`observed_at`, `created_at`, `expired_at`, `class`, `origin`, `natural_key`,
`confidence`, `source_quote`, `subject`, `run_id`, `session_id`, and after a
verdict `judged_at`, `withheld`, `judge_reason`, `judged_by`. Times are unix
nanoseconds.

## Errors

- `get_chunk: missing required field: id`.
- `get_chunk: no such chunk: <id>` — wrong id, a document id, or a chunk from
  another scope. Find the id with `search` or `query_chunks` rather than
  guessing.
- `get_chunk: body: ...` — the body could not be read; retrying is reasonable.

## Examples

Read a chunk before editing it:

```json
{"op": "get_chunk", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00"}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "document_id": "b4407b522dc11495d3de371311db17f0", "parent_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "position": 0, "title": "Timeline", "type": "section", "revision": 3, "body": "Beta on **1 March**, GA on 1 April.", "tags": ["release"]}
```

Read a fact in the tenant store, with its `entity` block:

```json
{"op": "get_chunk", "id": "0f1e2d3c4b5a69788796a5b4c3d2e1f0", "scope": "tenant"}
```
