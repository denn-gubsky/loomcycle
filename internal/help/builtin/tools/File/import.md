---
name: File/import
description: "File op=import — build a Document from a Markdown file in a volume, such as one extract wrote, with its pictures attached as image chunks."
---
`import` reads a Markdown file and builds a Document from it, as the Document
tool's `import_md` does: headings become the chunk hierarchy. The runtime
reads the file, so its text never passes through you, and each picture that
`extract` wrote (`volume:inbox:extracted/…`) becomes an image chunk holding
the picture's bytes.

The file is treated as someone else's text. A link in it to any other file —
in the inbox or in your own volumes — stays a link and is counted in
`skipped`; raw HTML and loom metadata comments in it are kept as text. The
document is tagged `untrusted` and `source/…` (where the file came from, e.g.
`source/mcp/ms365`).

Import writes a document, so it needs the Document tool as well as this one,
and the scope must be one your Document grants allow.

## Arguments

- `path` — the Markdown file, e.g. `volume:inbox:extracted/minutes.md`.
- `title` — the document's title. Default: the file's first heading.
- `doc_path` — where to name the document in the Path tree, e.g.
  `/board/2026/minutes`.
- `scope` — `agent`, `user` (the default) or `tenant`.
- `tags` — tags for the document, besides `untrusted` and the `source/…` tag.

## Returns

`{document_id, root_chunk_id, chunks, image_chunks, skipped, skipped_images,
source, tags}` — `skipped` counts the pictures that stayed links, and
`skipped_images` says why for the first of them.

## Errors

- You do not hold the Document tool, or a Document hook refuses the import.
- The file is not UTF-8 text, or is over 32 MiB.
- The scope is not one your Document grants allow.

## Examples

Import the protocol extract wrote, named in the board's folder:

```json
{"op": "import", "path": "volume:inbox:extracted/Protokol-zasidannya.md", "doc_path": "/board/2026/protokol-10", "tags": ["board"]}
```

```json result
{"document_id": "d2adb0dc0731a5a6f019fc808854ea28", "root_chunk_id": "1f38094d118618fa34c8c0873e06a704",
 "chunks": 7, "image_chunks": 1, "skipped": 0, "source": "mcp:ms365/get-attachment",
 "tags": ["board", "source/mcp/ms365", "untrusted"]}
```
