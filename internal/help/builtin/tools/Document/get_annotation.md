---
name: Document/get_annotation
description: "Document op=get_annotation — read the annotations of a section or a document, each marked stale or current."
---
Returns the annotations stored for one chunk: a section's `branch`
annotation, or a document's `document` annotation on its root chunk. Each one
says whether it is `stale`, computed now: true when the text it was written
from has changed since. A stale annotation is still the best summary there
is; read the text when the difference matters.

To get annotations with ordinary reads instead, pass `with_summary: true` to
`get_document`, `get_chunk`, `search`, `related`, `graph_recall`,
`documents_summary` or `query_documents`.

## Arguments

- `id` — the chunk: a section, or a document's root chunk.
- `document_id` — a document, for its `document` annotation.
- `path` — the document's Path-tree name, instead of `document_id`.
- `level` — `branch` or `document`, to read only that one.
- `scope` — `tenant`.

## Returns

`{"annotations": [{chunk_id, document_id, level, brief, abstract, language,
class?, inputs_sha256, prompt_version, model?, manual, written_at,
written_by, stale}, …]}` — empty when the chunk has none.

## Errors

- `get_annotation: no such chunk: …` / `no such document: …`.

## Examples

```json
{"op": "get_annotation", "scope": "tenant", "document_id": "71c8d1ce942bf85e3f49a4f00a5b7ff6"}
```

```json result
{"annotations": [{"chunk_id": "8709a7ba6adb5f40941582f4cc6754b4", "document_id": "71c8d1ce942bf85e3f49a4f00a5b7ff6", "level": "document", "brief": "A guide to installing the widget.", "abstract": "The guide covers unpacking, connecting and the conditions of use.", "language": "en", "class": "Guide", "inputs_sha256": "4c7e…", "prompt_version": 1, "manual": false, "written_at": "2026-11-09T22:40:00Z", "written_by": "run:9f2c", "stale": false}]}
```
