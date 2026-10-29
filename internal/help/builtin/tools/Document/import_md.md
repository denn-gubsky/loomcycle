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
- `reimport` — the Markdown is a new edition of the document named by
  `document_id` or `work_key`; see "A new edition" below.
- `work_key` — with `reimport`: the work's key, e.g. `law:ua/2297-vi`.
- `edition_date` — with `reimport`: when the edition came into force (unix
  nanoseconds; default now).
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.
- `source_url`, `source_kind`, `source_title` — new document only: where the
  text came from, such as the email whose body you are importing. Naming any
  of them marks the document as imported: its text is then found by document
  search but never treated as something you learned, and its root records the
  source as `provenance` (shown by `get_document` and `export_md`).
  `source_url` is the item's http(s) web link — an email's `webLink`, a
  SharePoint file's `webUrl` — never a download or upload link; at most 2048
  bytes, with no user name or password, and a signature or token in its query
  is masked before it is kept. `source_kind` is `sharepoint`, `email`,
  `attachment`, `web` or `other` (the default) and tags the document
  `source/<kind>`. `source_title` is the source's own name, such as the
  email's subject: one line, at most 300 characters.

Markdown written by `export_md` with metadata also carries:

- a `<!-- loom: {"id":"…","type":"task","status":"open","fields":{…}} -->`
  line directly under a heading — sets that chunk's type, status and fields,
  and `"valid_at"` / `"invalid_at"` (unix nanoseconds) the period it is true.
  Without them the chunk is undated — leave them out rather than guess. A
  period whose `invalid_at` is not after its `valid_at` refuses the whole
  import before anything is written.
  The `id` is only used to reconnect links: every imported chunk gets a NEW id.
- a `<!-- loom-edges: ... -->` block of `from -> to [kind]` lines — each edge
  whose two ends were both imported is recreated between the new chunks.

A body that is only a ```` ```mermaid ```` fence becomes a mermaid chunk; a
body that is only a `![caption](data:image/png;base64,…)` becomes an image
chunk. So does a body that is only `![caption](volume:<name>:<path>)` (or
`volume:<path>` for your default volume), on one line: the runtime reads the
file, so its bytes never pass through you. That read is yours: it needs the
Read tool and a volume you are bound to, and Read's hooks apply. One import
reads at most 64 such pictures and 64 MiB of them; a picture that cannot be
read, is not a PNG, JPEG, GIF or WebP, or is over the size limit stays a
plain link. Tags are not read from Markdown; add them afterwards with
`add_tags`.

## A new edition: `reimport`

When the Markdown is a new edition of a document you already hold — an amended
law, a revised standard — pass `reimport: true` with the document's
`document_id` or its `work_key`. Each part is recognised by its anchor (its
heading path, e.g. `article-2/paragraph-1`) and by its text, and reconciled
instead of recreated. Text is matched first, then headings:

- a part with the same heading and the same text is left untouched — its id,
  links, evidence and tags stay;
- a part whose text now sits under another heading (renumbered or moved) keeps
  its id and everything linked to it, and is still found by its old anchor. So
  inserting an article and renumbering the ones after it moves them; it does not
  hand one article's links to the next;
- a part with the same heading and new text is **superseded**: the old version
  closes at `edition_date` (it is still what the work said before) and a new one
  opens there, joined by `supersedes`. Links follow it where their evidence still
  holds; the others stay on the old version, marked `proposed` for re-checking.
  Inline `[[name]]` links are not carried: the new version links what its own
  text names;
- a part the edition no longer has closes; nothing is deleted;
- a new part opens at `edition_date`;
- the document's title and the text under its `#` heading (a preamble) are
  updated in place; the earlier text stays readable with `history` and
  `get_version` on the root chunk.

`edition_date` is when the edition came into force (unix nanoseconds; default
now). Import editions in date order: an `edition_date` earlier than the edition
in force is refused, and nothing is written. With `work_key` and no document
holding it yet, the first edition is imported as a new document carrying that
key — or onto the placeholder `resolve_anchor` filed for it, which then takes the
work's title. Running the same reimport twice changes nothing, and a reimport
that stopped part way is completed by running it again. Two reimports of the
same work run one after the other.

A reimport reads only the headings, the text under them, and a chunk's type,
status and fields from its `<!-- loom: ... -->` line. It ignores the
`<!-- loom-edges: ... -->` block, the `valid_at` / `invalid_at` in a `loom:`
line (a part's period comes from `edition_date`), and pictures: an image part
arrives as its caption text, without the image. Add those afterwards with
`link_chunks`, `update_chunk` and `set_asset`.

The response adds `reimport: {unchanged, superseded, moved, closed, created,
statements_copied, statements_proposed}` and `changed_chunk_ids` (the new,
superseding and moved parts, and the root when its title or text changed).

## Returns

`{document_id, root_chunk_id, chunks_created, image_chunks, skipped,
skipped_images}`, plus `provenance` for an import that names a source and
`not_announced` when the import was not published to the scope's
`documents/ingested` channel (your access-control rules hide part of the scope
from reading, so the event could name the document to readers who may not see
it) —
`skipped` counts the pictures that stayed links and
`skipped_images` says why for the first of them. For an import into an existing
document, `root_chunk_id` is the chunk you imported under. The new document's
path is not in the result; use `get_document` or `Path` `ls` to see it.

## Errors

- `import_md: missing required field: markdown`.
- `import_md: no headings found — a document needs at least one '# Heading'` —
  add a heading; plain text alone has nothing to become a chunk.
- `import_md: no such document: ...` / `no such parent chunk: ...` — the
  target is not in this scope.
- A source that is not valid (a URL that is not http(s), has a user name or
  password, or is too long; a `source_kind` not in the list), or a source with
  `document_id`: a source describes a whole new document. Nothing is written.
- With `reimport`: `edition_date ... is earlier than the edition already in
  force` — import editions in date order. `identity_collision` — another
  document holds the `work_key`.
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

Import an email's body, linked back to the message:

```json
{"op": "import_md", "path": "/mail/2026/board-minutes", "source_url": "https://outlook.office365.com/owa/?ItemID=AAMkAGI2&exvsurl=1&viewmodel=ReadMessageItem", "source_kind": "email", "source_title": "Board minutes, 21 October", "markdown": "# Board minutes, 21 October\n\nThe budget was approved."}
```

Set a chunk's type and status on the way in:

```json
{"op": "import_md", "path": "/docs/tasks", "scope": "agent", "markdown": "# Tasks\n\n## Rotate the API keys\n<!-- loom: {\"type\":\"task\",\"status\":\"open\"} -->\n\nBefore Friday."}
```
