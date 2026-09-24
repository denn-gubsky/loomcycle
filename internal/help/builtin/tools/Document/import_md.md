---
name: Document/import_md
description: "Document op=import_md — build chunks from Markdown: headings become the chunk tree, as a new document or under a chunk of an existing one."
---
`import_md` turns Markdown into chunks. **Every heading becomes one chunk**;
the text under a heading, up to the next heading, becomes that chunk's body;
a deeper heading becomes a child of the nearest shallower heading above it.
Use it to write a whole structured document in one call, or to bring back
something `export_md` produced.

**Omit `document_id` to create a NEW document; pass it to add the chunks to an
existing one.** A new document always gets a Path-tree name.

## Arguments

- `markdown` (required) — the Markdown. It needs at least one `#`–`######`
  heading. Headings inside fenced code blocks are left as text.
- `document_id` — import into this existing document. Omitted: create a new
  document whose root chunk is the FIRST heading (its text is the title; the
  `title` argument is not used). Every later heading, even another `#`, goes
  under that root.
- `parent_id` — with `document_id`: import under this chunk. Default: under
  the document's root chunk.
- `path` — new document only: its Path-tree name, e.g. `/docs/launch`.
  Default: `/documents/<first heading, slugified>`. That default is shared by
  any other document with the same title in this scope — the name then points
  at the newest one — so pass `path` when the title is not unique.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.

Markdown written by `export_md` with metadata also carries:

- a `<!-- loom: {"id":"…","type":"task","status":"open","fields":{…}} -->`
  line directly under a heading — sets that chunk's type, status and fields.
  The `id` is only used to reconnect links: every imported chunk gets a NEW id.
- a `<!-- loom-edges: ... -->` block of `from -> to [kind]` lines — each edge
  whose two ends were both imported is recreated between the new chunks.

A body that is only a ```` ```mermaid ```` fence becomes a mermaid chunk; a
body that is only a `![caption](data:image/png;base64,…)` becomes an image
chunk. Tags are not read from Markdown; add them afterwards with `add_tags`.

## Returns

`{document_id, root_chunk_id, chunks_created}`. For an import into an existing
document, `root_chunk_id` is the chunk you imported under. The new document's
path is not in the result; use `get_document` or `Path` `ls` to see it.

## Errors

- `import_md: missing required field: markdown`.
- `import_md: no headings found — a document needs at least one '# Heading'` —
  add a heading; plain text alone has nothing to become a chunk.
- `import_md: no such document: ...` / `no such parent chunk: ...` — the
  target is not in this scope.
- The import is not all-or-nothing. If it fails part-way, the chunks created so
  far stay; for a new document, `delete_document` it and import again.
- In the tenant scope, the shared ontology document refuses imports from
  agents.

## Examples

Create a new document with a named path:

```json
{"op": "import_md", "path": "/docs/launch", "markdown": "# Launch plan\n\nShip on the 14th.\n\n## Migrate the database\n\nRun the migration in two steps.\n\n## Announce\n\nPost to the changelog."}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "root_chunk_id": "5f1c9e2a7b3d4e6f8a9b0c1d2e3f4a5b", "chunks_created": 3}
```

Add a section, with its own sub-section, under an existing chunk:

```json
{"op": "import_md", "document_id": "b4407b522dc11495d3de371311db17f0", "parent_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "markdown": "## Rollback plan\n\nRestore from the snapshot.\n\n### Who decides\n\nThe on-call lead."}
```

Set a chunk's type and status on the way in:

```json
{"op": "import_md", "path": "/docs/tasks", "scope": "agent", "markdown": "# Tasks\n\n## Rotate the API keys\n<!-- loom: {\"type\":\"task\",\"status\":\"open\"} -->\n\nBefore Friday."}
```
