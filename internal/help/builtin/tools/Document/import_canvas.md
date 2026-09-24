---
name: Document/import_canvas
description: "Document op=import_canvas — create a NEW document from a JSON Canvas object: each node becomes a chunk, each edge a link."
---
`import_canvas` builds a new document from a JSON Canvas v1.0 object — the
contents of an Obsidian `.canvas` file, or what `export_canvas` returned. Every
node becomes a chunk directly under the new document's root, keeping its board
position; every edge becomes a link between those chunks. It always creates a
new document: it cannot add to an existing one.

## Arguments

- `canvas` (required) — the object `{"nodes": [...], "edges": [...]}`, passed
  as JSON, not as a string.
  - A node needs `id` and `type` and should have `x`, `y`, `width`, `height`
    (integers). By type: `text` → body is `text`, title is its first line;
    `file` → body `[file: <file>]`; `link` → body is the `url`; `group` →
    title is its `label`, no body. Groups do not nest their members: every
    chunk sits directly under the root.
  - An edge needs `fromNode` and `toNode`. Its `label` becomes the link kind
    (default `references`). An edge naming a missing node is skipped.
- `title` — the document title. Default `Imported canvas`.
- `path` — its Path-tree name. Default `/documents/<title, slugified>`, which
  is shared with any same-titled document in this scope, so pass a `path` when
  the title is not unique.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{document_id, root_chunk_id, chunks_created, edges_created}`. The path is not
in the result; `get_document` shows it.

## Errors

- `import_canvas: missing required field: canvas`.
- `import_canvas: invalid canvas JSON: ...` — the object does not fit the
  canvas shape, e.g. a coordinate is a string or a fraction. Fix the object;
  resending it unchanged fails the same way.
- The import is not all-or-nothing: on a failure part-way, `delete_document`
  the new document and try again.

## Examples

Import a two-node canvas as a new document:

```json
{"op": "import_canvas", "title": "Launch board", "path": "/boards/launch", "canvas": {"nodes": [{"id": "n1", "type": "text", "text": "Migrate the database", "x": 0, "y": 0, "width": 400, "height": 200}, {"id": "n2", "type": "text", "text": "Announce", "x": 450, "y": 0, "width": 400, "height": 200}], "edges": [{"id": "e1", "fromNode": "n1", "toNode": "n2", "label": "blocks"}]}}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "root_chunk_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "chunks_created": 2, "edges_created": 1}
```
