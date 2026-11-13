---
name: Document/lineage
description: "Document op=lineage — where a chunk sits: its enclosing sections, its document and every folder above the document's Path names, each with its brief, in one call."
---
`lineage` answers "where is this?" for a chunk you found by search or by a
walk: the sections that enclose it, the document it belongs to, and each
folder above the document's Path name up to `/`, each with its annotation
brief when it has one. Use it to step UP from a hit — to see what the
surrounding section, document and folder are about — instead of calling
`get_chunk`, `get_document` and Path `ls` one after another. It reads no
bodies; to read text, call `get_chunk` on the id you choose.

## Arguments

- `id` (required) — the **chunk** id. A document's `root_chunk_id` asks about
  the document itself (no sections); a `document_id` here is "no such chunk".
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

```
{id,
 sections: [{id, title, type?, brief?, stale?}, …],
 document: {document_id, root_chunk_id, title, type?, status?, brief?, stale?},
 paths: [{path, folders: [{path, brief?}, …]}, …],
 sections_truncated?, paths_truncated?}
```

- `sections` — nearest first: the chunk itself (unless it is the root), then
  each enclosing section below the document's root. At most 64; a deeper chunk
  returns the nearest 64 and `sections_truncated: true`, which is also set when
  the document's tree is broken above some section (the walk stops there).
- `document` — the document the chunk belongs to, with its own brief.
- `paths` — one entry per Path name of the document, in sort order, at most 4
  (`paths_truncated: true` when it has more). Each lists its folders nearest
  first, up to `/`; a folder you may not list is left out, so the chain can
  stop short of `/` or skip a level. A document filed under no name has
  `paths: []`.
- `brief` — the annotation's one-sentence summary, present only when one has
  been written. `stale: true` on a section or the document means the text its
  brief was written from has changed since; it is still the best summary there
  is. Folders carry no `stale` mark: to know whether a folder's brief is
  current, ask `get_annotation` with `level: path`.

What you are shown follows your access: a folder you could not list with Path
`ls` is left out of the chain, and a folder's brief is left out when you may
not read every document it covers (the folder itself stays).

## Errors

- `lineage: missing required field: id`.
- `lineage: no such chunk: <id>` — wrong id, a document id, or a chunk from
  another scope. A chunk you may not read is refused as `get_chunk` refuses it.

## Examples

Step up from a search hit to its section, document and folders:

```json
{"op": "lineage", "scope": "tenant", "id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00"}
```

```json result
{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "sections": [{"id": "5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00", "title": "Step one"}, {"id": "2a7c9e0b1d3f4a5b6c7d8e9f0a1b2c3d", "title": "Setup", "brief": "How to unpack and connect the widget.", "stale": true}], "document": {"document_id": "b4407b522dc11495d3de371311db17f0", "root_chunk_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "title": "Widget guide", "type": "Guide", "brief": "A guide to installing the widget."}, "paths": [{"path": "/kb/widgets/guide", "folders": [{"path": "/kb/widgets", "brief": "Everything about the widget product line."}, {"path": "/kb", "brief": "The team's knowledge base."}, {"path": "/"}]}]}
```
