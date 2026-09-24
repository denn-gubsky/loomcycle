---
name: Document/get_edges
description: "Document op=get_edges — every edge touching a document's chunks, in or out, with both endpoints' titles, types and documents."
---
`get_edges` lists every cross-reference edge that starts or ends at any chunk
of one document, so you can see the document's whole link graph in one call.
**It takes `document_id`, not a chunk `id`.** For the edges pointing at ONE
chunk, use `backlinks`.

## Arguments

- `document_id` (required) — the document.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{edges: [{from_id, to_id, kind, auto, from_title, from_type, from_status,
from_document_id, to_title, to_type, to_status, to_document_id}]}`, ordered by
kind, then age. `auto: true` marks an edge made by a `[[name]]` link in a body;
`false` is one made with `link_chunks`. The endpoint fields appear only when
they have a value. The list is not paged.

## Errors

- `get_edges: missing required field: document_id` — passing the id as `id`
  does not work here.
- An unknown document id is not an error: it returns `{edges: []}`. Check the
  id and the scope before concluding a document has no links.

## Examples

All links in and out of a document:

```json
{"op": "get_edges", "document_id": "b4407b522dc11495d3de371311db17f0"}
```

```json result
{"edges": [
  {"from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "kind": "blocks", "auto": false,
   "from_title": "Migrate the database", "from_document_id": "b4407b522dc11495d3de371311db17f0",
   "to_title": "Launch", "to_document_id": "b4407b522dc11495d3de371311db17f0"}]}
```
