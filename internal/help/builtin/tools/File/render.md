---
name: File/render
description: "File op=render — write a Document you may read, or a Markdown file, as a file to send: Word (DOCX), HTML or Markdown, into the inbox or one of your volumes."
---
`render` is the reverse of `import`: it writes a Document as a file people
open, send and print. The runtime reads the document and writes the file, so
neither its text nor the file's bytes pass through you; you get a reference.
From the inbox, `keep` copies the file to a volume and `upload` sends it on.

The document is read as `Document op=export_md` reads it, with your scopes
and access: you need the Document tool, and a read-only Document is enough.
A link to another part of the same document becomes a link inside the file;
a link to another document becomes its Web UI address when the server has a
public URL and you may read it, and plain text otherwise. Only http, https and
mailto links are kept, and text in the document never becomes markup or
script in the file. A document whose status is `draft` or `review` carries a
small Draft or Review label beside its title.

Every render of a document is logged on it: `Document op=get_document` shows
the newest render per format, and `op=history` with `include: ["renders"]` on
the root chunk lists them. When an earlier render of the same content and
options is still unchanged in a volume, it is reused rather than written
again (with `to`, copied there).

## Arguments

- `document` — the id of the document to render.
- `doc_path` — or the document's name in the Path tree, e.g. `/board/2026/minutes`.
- `path` — or a Markdown file in the inbox or a volume; its first `#` heading
  is the title. For an HTML file, run `extract` first and render its Markdown.
- `scope` — the document's store: `user` (default), `agent` or `tenant`.
- `chunk` — render only the part of the document under this chunk, titled by it.
- `format` — `docx` (a Word document: headings in Word's own heading styles,
  lists, tables, pictures, links, and the status in its properties), `html`
  (one self-contained page, pictures inside it) or `md` (clean Markdown, its
  pictures as files in a folder beside it). `pdf` is not available yet.
- `to` — a file in one of your read-write volumes. Its folder must exist.
  Default: `volume:inbox:rendered/<title>.<format>`.
- `overwrite` — replace a file already at `to`. Default false.
- `toc` — add a table of contents after the title.
- `title_page` — put the title on a page of its own.
- `provenance` — for an imported document, say under the title where it came
  from. Default true.
- `language` — the document's language, e.g. `uk`, for the file's properties.
- `page_size` — `a4` (default) or `letter`, for the paged formats.

Pass exactly one of `document`, `doc_path` and `path`.

## Returns

`{ref, format, media_type, size, sha256, render_id, reused, document_id,
report}`. `ref` is the file; `render_id` its row in the document's render log;
`reused` is true when an earlier, unchanged file was used. `report` counts
`sections`, `images` and `tables`, and what was left out: `images_skipped`
(a picture that is not PNG, JPEG, GIF or WebP, too large, or only linked),
`bodies_cut` (past 32 MiB of text) and `links_flattened`. A Markdown render
also returns `image_refs`, the pictures written beside it.

## Errors

- You do not hold the Document tool, or a Document hook refuses the export.
- The document or chunk is not there, or not readable to you.
- `to` names the inbox, a read-only volume or one you are not bound to, or a
  file that exists without `overwrite`.
- `format` is `pdf`, which this server does not write yet.
- The file would pass 64 MiB: render a part of the document with `chunk`.

## Examples

The board minutes as a Word document to send:

```json
{"op": "render", "doc_path": "/board/2026/minutes", "scope": "tenant", "format": "docx"}
```

The same as a web page, with a table of contents:

```json
{"op": "render", "doc_path": "/board/2026/minutes", "scope": "tenant", "format": "html", "toc": true}
```

One section as Markdown, kept in the `reports` volume:

```json
{"op": "render", "document": "4f1c…", "chunk": "9a0e…", "format": "md", "to": "volume:reports:2026/decisions.md"}
```
