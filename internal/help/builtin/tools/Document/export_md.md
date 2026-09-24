---
name: Document/export_md
description: "Document op=export_md — render a whole document as Markdown, either round-trippable (with metadata comments) or clean for people."
---
`export_md` turns a document into one Markdown text: each chunk becomes a
heading followed by its body, nested by depth (the root is `#`, its children
`##`, and so on). Use it to read a whole document at once, to hand it to a
person, or to copy it — `import_md` reads the output back.

**Choose `include_metadata` for the reader.** The default (`true`) adds
`<!-- loom: ... -->` comments that let `import_md` rebuild types, statuses,
fields and links; pass `false` for clean Markdown a person will read.

## Arguments

- `document_id` — the document. Instead you may pass `id` (the document id)
  or `path` (its Path-tree path, e.g. `/docs/launch`).
- `include_metadata` — default `true`: after each heading a
  `<!-- loom: {"id":…,"type":…,"status":…,"fields":…} -->` line, and at the end
  a `<!-- loom-edges: ... -->` block listing the links that start in this
  document. `false`: no comments, and every `![[...]]` embed in a body is
  replaced by the embedded chunk's text.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

## Returns

`{markdown, document_id, title}`.

Headings stop at `######`: chunks nested deeper than six levels all render at
level six, so re-importing such a document flattens them. Tags are not part of
the export. A mermaid chunk renders as a ```` ```mermaid ```` fence and an
image chunk as an inline base64 data URL, which can make the output large.

## Errors

- `export_md: missing required field: id (or path)` — you passed neither
  `document_id`, `id` nor `path`.
- `export_md: no such document: ...` / `no such path: ...` — not in this
  scope. Look it up with `Path` `ls` or `query_documents`.
- `export_md: body: ...` — a chunk body could not be read; the export stops
  rather than return a document with missing text. Retrying can help.

## Examples

Clean Markdown to show a person:

```json
{"op": "export_md", "path": "/docs/launch", "include_metadata": false}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "title": "Launch plan",
 "markdown": "# Launch plan\n\nShip on the 14th.\n\n## Migrate the database\n\nRun the migration in two steps.\n\n"}
```

A round-trippable copy, for `import_md` into another scope or document:

```json
{"op": "export_md", "document_id": "b4407b522dc11495d3de371311db17f0"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "title": "Launch plan",
 "markdown": "# Launch plan\n<!-- loom: {\"id\":\"5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b\"} -->\n\nShip on the 14th.\n\n## Migrate the database\n<!-- loom: {\"id\":\"9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f\",\"type\":\"task\",\"status\":\"open\"} -->\n\nRun the migration in two steps.\n\n<!-- loom-edges:\n9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f -> 5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b [references]\n-->\n"}
```
