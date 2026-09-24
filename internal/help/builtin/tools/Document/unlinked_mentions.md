---
name: Document/unlinked_mentions
description: "Document op=unlinked_mentions — chunks whose body text contains a chunk's title but that do not link to it yet."
---
`unlinked_mentions` finds the places that NAME a chunk without linking to it:
every chunk in the scope whose body contains the target chunk's title (case
does not matter), minus the chunks that already link to it. Use it to turn
plain mentions into links — then `link_chunks`, or edit the mention into
`[[Title]]`. It matches the title text literally; for similar meaning, use
`related`.

## Arguments

- `id` (required) — the chunk whose title to look for.
- `limit` — at most this many, default 50, capped at 200.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{unlinked_mentions: [{chunk_id, title, document_id}], truncated}`.
`truncated: true` means the list is not complete — the limit was reached or the
scope holds more chunks than one scan covers — so there may be more.

A short or common title ("Notes", "Plan") matches almost everything; the
results are only useful for a distinctive title.

## Errors

- `unlinked_mentions: missing required field: id`.
- `unlinked_mentions: no such chunk: ...` — the chunk is not in this scope.
- `unlinked_mentions: target chunk has no title to match on`.

## Examples

Where is the "Rollback plan" mentioned without a link?

```json
{"op": "unlinked_mentions", "id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f"}
```

```json result
{"unlinked_mentions": [
  {"chunk_id": "e1f3a5b7c9d14e2f8a4b6c8d0e2f4a6b", "title": "Release notes", "document_id": "f0e1d2c3b4a5968778695a4b3c2d1e0f"}],
 "truncated": false}
```
