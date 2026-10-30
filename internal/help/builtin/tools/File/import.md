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

A file you changed after it was captured is no longer the captured file, so
the document records none of the above: the result says so in
`provenance_note`, and the document gets no `source/…` tag (a `source` link
you give is still kept). To add structure to a file and keep its record, use
`headings` instead of rewriting the file.

**Adding structure.** A text exported from a PDF often shows its structure
only by numbering ("Стаття 5. …", "3. Організаційна структура …"), never by
`#` headings, so it would import as one long chunk. `headings` adds the
headings for you, by line number, while the runtime reads the file unchanged:
the body is the file's own text, and the document keeps the file's record.
Each entry is one of:

- `{"line": N, "level": L, "as": "heading"}` — line N itself becomes a
  heading of level L. Add `"lines": 2` (up to 10) when its title wraps onto
  the next lines; they are joined with a space.
- `{"line": N, "level": L, "as": "marker", "title": "…"}` — a heading with
  `title` is put before line N, and line N stays in the body. Use it for a
  numbered paragraph (a пункт) whose first line is text, not a title.

Levels are 2 to 6 (the document's title is level 1). Give entries in
increasing line order, at most 10 000, and do not put one inside a heading's
wrapped lines. Lines count from 1 in the file as it is — read it, or its
outline, to find them. The document's `provenance` then also says
`structured: {headings, by}`, the count and the run that added them, so a
reader knows the headings were added, not written by the file's author.

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
- `headings` — headings to add, by line number: `{line, level, as}` with
  `as` `heading` (plus `lines` for a wrapped title) or `marker` (plus
  `title`, one line, at most 300 characters). See above.
- `reimport` — the file is a new edition of the work named by `work_key`; see
  "A new edition" below.
- `work_key` — with `reimport`: the work's key, e.g. `law:ua/2297-vi`.
- `edition_date` — with `reimport`: when the edition came into force (unix
  nanoseconds; default now).
- `amending_document_id` — with `reimport`: the document of the same scope
  that made the edition, such as the amending act.

## A new edition

With `reimport: true` and the work's `work_key`, the file is reconciled onto
the document holding that key part by part, exactly as the Document tool's
`import_md` with `reimport` does (its help article has the rules): unchanged and
moved parts keep their ids and links, changed parts are superseded at
`edition_date`, removed parts close, and nothing is deleted. When no document
holds the key yet, the file is imported as its first edition. With
`amending_document_id`, each part the edition renumbered gets an `amends`
statement from that document. The scope's `documents/ingested` channel is told
the document was `reimported`.

A reimport reads the file's headings and text; pictures are not attached to
the parts it changes. `doc_path` and `source` are used only by a first edition. The document still gets the
`untrusted` and `source/…` tags.

## Returns

`{document_id, root_chunk_id, chunks, image_chunks, skipped, skipped_images,
source, provenance, provenance_note, tags}` — `skipped` counts the pictures
that stayed links, and `skipped_images` says why for the first of them.
`provenance` is what the document records of where it came from;
`get_document` shows it too. `provenance_note` is there only when the file was
changed after it was captured.

A reimport returns `{document_id, root_chunk_id, reimport, edition_date,
work_key, changed_chunk_ids, source, provenance_note, tags}` instead of the
chunk counts: `reimport` is `import_md`'s report of what the edition changed,
and `not_announced` says why the scope was not told, when it was not.

## Errors

- You do not hold the Document tool, or a Document hook refuses the import.
- The file is not UTF-8 text, or is over 32 MiB.
- The scope is not one your Document grants allow.
- `source` is not valid: a URL that is not http(s), has a user name or
  password, or is too long; or a `kind` not in the list.
- `headings` is not valid: a line outside the file, entries out of order or
  inside a heading's wrapped lines, a level not 2 to 6, a heading on a blank
  line, a marker without a one-line title, or a title over 300 characters.
  Nothing is imported.

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

Import a law whose articles are only numbered, each article a heading (the
title of article 2 wraps onto a second line):

```json
{"op": "import", "path": "volume:inbox:extracted/Zakon-pro-banky.md", "scope": "tenant", "doc_path": "/board/regulations/zakon-pro-banky",
 "source": {"url": "https://tenant.sharepoint.com/sites/board/Shared%20Documents/zakon.pdf", "kind": "sharepoint"},
 "headings": [
   {"line": 12, "level": 2, "as": "heading"},
   {"line": 31, "level": 2, "as": "heading", "lines": 2},
   {"line": 58, "level": 2, "as": "heading"}
 ]}
```

Import a regulation by sections, with each numbered пункт as a marker under
its section, so the пункт's own text stays in the body:

```json
{"op": "import", "path": "volume:inbox:extracted/NBU-064.md", "scope": "tenant", "doc_path": "/board/regulations/nbu-064",
 "headings": [
   {"line": 593, "level": 2, "as": "heading"},
   {"line": 594, "level": 3, "as": "marker", "title": "1. Це Положення розроблено…"},
   {"line": 601, "level": 3, "as": "marker", "title": "2. Банк забезпечує…"}
 ]}
```

```json result
{"document_id": "8c1e…", "root_chunk_id": "41d2…", "chunks": 4, "image_chunks": 0, "skipped": 0,
 "source": "mcp:ms365/download-bytes",
 "provenance": {"origin": "import", "file": "NBU-064.pdf", "sha256": "9f3a…", "captured_from": "mcp:ms365/download-bytes",
                "captured_at": "2026-10-28T07:12:40Z", "run_id": "run_7c2…", "structured": {"headings": 3, "by": "run_7c2…"}},
 "tags": ["source/mcp/ms365", "untrusted"]}
```
