---
name: Document/update_chunk
description: "Document op=update_chunk — change a chunk's title, type, status, body, fields or tags; requires the chunk's current revision."
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
  `status`.
- `body` — the new Markdown body, replacing the old one whole. Recorded in the
  chunk's history.
- `fields` — the new fields object, replacing the old one whole (not merged):
  send every field you want to keep.
- `tags` — the new tag set, replacing the old one; `[]` clears all tags.
- `scope` — `user` (default), `agent` or `tenant`.

Leave a key out to keep its value. Moving a chunk is `move_chunk`, not this.

## Returns

The updated chunk, as `get_chunk` returns it, with the new `revision`. Use that
revision for your next update.

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
- `an agent may not modify the ontology document ...` — the tenant ontology is
  operator-only.

## Examples

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
