---
name: Document/graph_snapshot
description: "Document op=graph_snapshot — a whole scope's documents, chunk trees, relations and Path names as parallel columns, paged by whole documents."
---
`graph_snapshot` exports a scope's structure in one columnar answer: its
documents, every chunk of each one as a tree, the relations between chunks,
the Path-tree names, and which chunks are facts. It carries titles and
metadata, never bodies — it is for drawing or mirroring a scope, not for
reading text (use `get_chunk`, `export_md` or `search` for that). A page always
holds whole documents; page on with `after` until `page.next_cursor` is absent.

## Arguments

- `scope` — `user` (default), `agent` or `tenant`.
- `include` — the sections to return: any of `documents`, `chunks`, `edges`,
  `dirents`, `facts`, `projection`. Default: all but `projection`. `chunks`
  needs `documents`; `edges`, `facts` and `projection` need `chunks`.
- `under_path` — only documents named at or under this Path-tree path.
- `document_ids`, `chunk_ids` — only these documents, or the documents holding
  these chunks (the union of both; at most 5000 ids each).
- `types`, `type` — only documents of these types (the root chunk's), each with
  its subtypes.
- `statuses`, `status` — only documents with any of these statuses.
- `tag`, `tag_prefix` — only documents carrying this tag, or a tag equal to or
  nested under the prefix (the document's own tags, not its chunks').
- `as_of` — only relations whose own validity contains this instant (unix
  nanoseconds). Documents and chunks are not filtered by it.
- `page_documents` — documents per page: default 200, at most 1000 (a larger
  value is lowered, never refused).
- `after` — the previous page's `page.next_cursor` (the name `get_edges` and
  `list_statements` page with).
- `edge_budget` — the most relations one page returns: default 50000, at most
  200000.

Filters combine with AND. Documents come in id order.

## Returns

Every section is parallel arrays: position `i` of each array describes the same
item. An `*_idx` column is a position in another section of the same answer,
and a `*_code` column a position in a `codes` list (`-1` = none).

- `page` — `{documents, next_cursor?, clamped_to?}`. `clamped_to` says the
  SQL Memory row cap fitted fewer documents on this page; the next page starts
  where it stopped.
- `codes` — `{types, statuses, kinds, fact_classes, methods}`.
- `documents` — `ids, titles, root_chunk_ids, type_code, status_code,
  created_at, updated_at, color_enabled, color_scheme, paths` (each document's
  every Path name, shortest first).
- `chunks` — `ids, document_idx, parent_idx, position, type_code, status_code,
  titles, valid_at, invalid_at, updated_at`; with `facts` also `is_fact,
  fact_class_code, fact_confidence, fact_retired`. Each document's chunks are
  in reading order and a parent always comes before its children
  (`parent_idx` -1 = a root).
- `edges` — `ids, from_idx, to_idx, kind_code, auto, created_at, status_code,
  method_code, confidence, valid_at, invalid_at, external, truncated`. Every
  live relation with an end on the page, once. An end on another page is a
  negative index naming an `external` entry `{idx, id, document_id}`.
  `truncated: true` means `edge_budget` was reached.
- `dirents` — first page only: `paths, kinds, document_idx, document_ids` —
  the directories and document names at or under `under_path`.
- `projection` — not computed yet: `{model: "", basis_version: 0, xyz: [],
  missing: [every chunk index]}`.

Timestamps are unix nanoseconds. A document the caller may not read is absent,
with its chunks, the relations into it and its Path names: the page and its
cursor are what they would be if it did not exist.

## Errors

- `graph_snapshot: unknown include ...` / `include "edges" needs "chunks"` —
  fix `include`.
- `graph_snapshot: after is not a cursor this op returned` — pass
  `page.next_cursor` unchanged as `after`, or drop `after` to start over.
- `graph_snapshot: document ... has more chunks than one SQL Memory query may
  return` — one document is larger than the operator's row cap; read it with
  `export_md`.

## Examples

The first page of everything in the tenant scope:

```json
{"op": "graph_snapshot", "scope": "tenant"}
```

```json result
{"page": {"documents": 1}, "codes": {"types": ["plan"], "statuses": ["asserted"], "kinds": ["cites"], "fact_classes": [], "methods": ["manual"]}, "documents": {"ids": ["b4407b522dc11495d3de371311db17f0"], "titles": ["Launch plan"], "root_chunk_ids": ["9e1c3a7d2b5f4e6081a2c3d4e5f60718"], "type_code": [0], "status_code": [-1], "created_at": [1772323200000000000], "updated_at": [1772409600000000000], "color_enabled": [false], "color_scheme": [null], "paths": [["/docs/launch"]]}, "chunks": {"ids": ["9e1c3a7d2b5f4e6081a2c3d4e5f60718", "3f2a9c1e7d6b4a5f8e0d1c2b3a4f5e6d"], "document_idx": [0, 0], "parent_idx": [-1, 0], "position": [0, 0], "type_code": [0, -1], "status_code": [-1, -1], "titles": ["Launch plan", "Goals"], "valid_at": [null, null], "invalid_at": [null, null], "updated_at": [1772409600000000000, 1772409600000000000], "is_fact": [false, false], "fact_class_code": [-1, -1], "fact_confidence": [null, null], "fact_retired": [false, false]}, "edges": {"ids": ["5d1e0c9b8a7f6e5d4c3b2a1908f7e6d5"], "from_idx": [1], "to_idx": [-1], "kind_code": [0], "auto": [false], "created_at": [1772409600000000000], "status_code": [0], "method_code": [0], "confidence": [null], "valid_at": [null], "invalid_at": [null], "external": [{"idx": -1, "id": "7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d", "document_id": "0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5f"}], "truncated": false}, "dirents": {"paths": ["/docs", "/docs/launch"], "kinds": ["directory", "document"], "document_idx": [-1, 0], "document_ids": ["", "b4407b522dc11495d3de371311db17f0"]}}
```

The next page, documents and chunks only, under `/kb`:

```json
{"op": "graph_snapshot", "scope": "tenant", "under_path": "/kb", "include": ["documents", "chunks"], "page_documents": 100, "after": "eyJkIjoiYjQ0MDdiNTIyZGMxMTQ5NWQzZGUzNzEzMTFkYjE3ZjAifQ"}
```
