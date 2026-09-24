---
name: Document/export_canvas
description: "Document op=export_canvas — render a document as a JSON Canvas (nodes and edges on a 2-D board, the Obsidian Canvas format)."
---
`export_canvas` returns a document as a JSON Canvas v1.0 object: one text node
per chunk and one edge per link between them, each node with a position and
size. Use it when someone wants a spatial, board-style view of a document, or
to open it in a tool that reads `.canvas` files. For a readable text version,
use `export_md`.

What is left out: the document's root chunk (it holds the title, not content),
and any link that leaves the document or touches the root. The hierarchy is
not drawn — nodes are laid out on a board, not as a tree.

## Arguments

- `document_id` — the document. Instead you may pass `id` (the document id)
  or `path` (its Path-tree path).
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{canvas: {nodes: [...], edges: [...]}, document_id}`.

- Node: `{id, type: "text", x, y, width, height, color?, text}`. `id` is the
  chunk id; `text` is its body, or its title when the body is empty. A chunk
  that came in through `import_canvas` keeps its board position; any other is
  placed on a fixed grid, four nodes per row.
- Edge: `{id, fromNode, toNode, toEnd: "arrow", label}`. `label` is the link's
  kind.

## Errors

- `export_canvas: missing required field: id (or path)` — you passed none of
  `document_id`, `id` or `path`.
- `export_canvas: no such document: ...` — not in this scope.

## Examples

Export a document as a canvas:

```json
{"op": "export_canvas", "document_id": "b4407b522dc11495d3de371311db17f0"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "canvas": {
  "nodes": [
    {"id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "type": "text", "x": 0, "y": 0, "width": 400, "height": 200, "text": "Run the migration in two steps."},
    {"id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "type": "text", "x": 450, "y": 0, "width": 400, "height": 200, "text": "Post to the changelog."}],
  "edges": [
    {"id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f-c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f-blocks", "fromNode": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "toNode": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "toEnd": "arrow", "label": "blocks"}]}}
```
