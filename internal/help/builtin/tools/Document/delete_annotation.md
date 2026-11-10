---
name: Document/delete_annotation
description: "Document op=delete_annotation — remove one annotation of a section or a document."
---
Removes the annotation of one chunk at one level. The document is not
touched. A driver granted `statement_methods: [annotate]` may delete what a
driver wrote; only the operator deletes an annotation written by hand.

Annotations are removed with their chunk on their own: deleting a chunk or a
document needs no call here.

## Arguments

- `id` — the chunk: a section, or a document's root chunk.
- `document_id` — a document, for its `document` annotation.
- `path` — the document's Path-tree name, instead of `document_id`; with
  `level: path`, the folder (its `_annotation` document is removed too).
- `level` (required) — `branch`, `document` or `path`.
- `scope` — `tenant`.

## Returns

`{"deleted": true}`, or `{"deleted": false}` when there was none.

## Errors

- `delete_annotation: missing required field: level`.
- `delete_annotation: manual_record: …` — only the operator deletes it.

## Examples

```json
{"op": "delete_annotation", "scope": "tenant", "id": "5c409a5230ada95068c0efda767832ba", "level": "branch"}
```

```json result
{"deleted": true}
```
