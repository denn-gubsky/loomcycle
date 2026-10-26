---
name: File/import
description: "File op=import — build a Document from a Markdown file in a volume, such as one extract wrote, with its pictures attached as image chunks."
---
`import` reads a Markdown file and builds a Document from it, as the Document
tool's `import_md` does: headings become the chunk hierarchy. The runtime
reads the file, so its text never passes through you, and each picture that
`extract` wrote (`volume:inbox:extracted/…`) becomes an image chunk holding
the picture's bytes. An image chunk is a chunk of its own, so each picture
is placed under its own sub-heading after the text of the section it was in;
a picture inside a table cell stays a link. A picture from a PDF or a slide deck
carries the page or slide it was on as the image chunk's `page` or `slide`
field. Image chunks hold PNG, JPEG, GIF and WebP: a JPEG 2000 picture from a
PDF stays a link and is counted in `skipped`.

The file is treated as someone else's text. A link in it to any other file —
in the inbox or in your own volumes — stays a link and is counted in
`skipped`; raw HTML and loom metadata comments in it are kept as text. The
document is tagged `untrusted` and `source/…` (where the file came from, e.g.
`source/mcp/ms365`).

The document records where it came from, and you cannot set or change this
yourself. When the file is one a tool returned (or text `extract` wrote from
one), and it has not been changed since, the document's root carries
`provenance`: the original file's name, type and sha256, where and when it
was captured (`captured_from`, `captured_at`) and by which run. Its text is
marked as imported, so it is found by document search and is never treated
as something you learned. A document whose text has no heading is titled
after the file's name.

Give `source` to keep a link back to where the file lives: the item's web
link — a SharePoint file's `webUrl`, an email's `webLink` — never a download
or upload link, which grants access to whoever holds it. A downloaded file
with no `source` links back to its download address, without the query.

Import writes a document, so it needs the Document tool as well as this one,
and the scope must be one your Document grants allow.

## Arguments

- `path` — the Markdown file, e.g. `volume:inbox:extracted/minutes.md`.
- `title` — the document's title. Default: the file's first heading.
- `doc_path` — where to name the document in the Path tree, e.g.
  `/board/2026/minutes`.
- `scope` — `agent`, `user` (the default) or `tenant`.
- `tags` — tags for the document, besides `untrusted` and the `source/…` tag.
- `source` — where the document came from: `{url, kind, title}`.
  `url` is an http(s) web link, at most 2048 bytes, with no user name or
  password; a signature or token in its query is masked before it is kept.
  `kind` is `sharepoint`, `email`, `attachment`, `web` or `other` (the
  default) and adds the tag `source/<kind>`. `title` is the source's own
  name, such as the email's subject: one line, at most 300 characters.

## Returns

`{document_id, root_chunk_id, chunks, image_chunks, skipped, skipped_images,
source, provenance, tags}` — `skipped` counts the pictures that stayed links,
and `skipped_images` says why for the first of them. `provenance` is what the
document records of where it came from; `get_document` shows it too.

## Errors

- You do not hold the Document tool, or a Document hook refuses the import.
- The file is not UTF-8 text, or is over 32 MiB.
- The scope is not one your Document grants allow.
- `source` is not valid: a URL that is not http(s), has a user name or
  password, or is too long; or a `kind` not in the list.

## Examples

Import an attachment's extracted text, linked back to the email it came with:

```json
{"op": "import", "path": "volume:inbox:extracted/Protokol-7.md", "source": {"url": "https://outlook.office365.com/owa/?ItemID=AAMkAGI2&exvsurl=1&viewmodel=ReadMessageItem", "kind": "email", "title": "Board minutes, 21 October"}}
```

Import the protocol extract wrote, named in the board's folder:

```json
{"op": "import", "path": "volume:inbox:extracted/Protokol-zasidannya.md", "doc_path": "/board/2026/protokol-10", "tags": ["board"]}
```

```json result
{"document_id": "d2adb0dc0731a5a6f019fc808854ea28", "root_chunk_id": "1f38094d118618fa34c8c0873e06a704",
 "chunks": 7, "image_chunks": 1, "skipped": 0, "source": "mcp:ms365/get-attachment",
 "tags": ["board", "source/mcp/ms365", "untrusted"]}
```
