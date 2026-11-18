---
name: File/render
description: "File op=render — write a Document you may read, or a Markdown file, as a file to send: PDF, Word (DOCX), HTML or Markdown, into the inbox or one of your volumes."
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
- `format` — `pdf` (typeset pages: page numbers, a table of contents with
  page numbers when `toc` is set, internal links, pictures; at most 2000
  pages), `docx` (a Word document: headings in Word's own heading styles,
  lists, tables, pictures, links, and the status in its properties), `odt`
  (an OpenDocument text file, the same content in LibreOffice's own styles),
  `epub` (an e-book: a page per top-level section, a contents list, pictures
  inside it), `html` (one self-contained page, pictures inside it) or `md`
  (clean Markdown, its pictures as files in a folder beside it).
- `to` — a file in one of your read-write volumes. Its folder must exist.
  Default: `volume:inbox:rendered/<title>.<format>`.
- `overwrite` — replace a file already at `to`. Default false.
- `toc` — add a table of contents after the title.
- `title_page` — put the title on a page of its own.
- `provenance` — for an imported document, say under the title where it came
  from. Default true.
- `style` — the house style to write the file in: the Path of a style
  document in the tenant's tree, or `none` for the built-in look. Default:
  the tenant's `/render/styles/default` when it exists. See "House styles"
  below.
- `metadata` — `embed` writes loomcycle's record of the document into the
  file: each chunk's id, type, status, tags and fields, and the relations
  inside the document, never its provenance. `File op=import` of such a file
  can then re-import onto the same document. Default `none`: a file people
  send carries no ids.
- `language` — the document's language, e.g. `uk`, for the file's properties.
- `page_size` — `a4` (default) or `letter`, for the paged formats.

Pass exactly one of `document`, `doc_path` and `path`.

## Returns

`{ref, format, media_type, size, sha256, render_id, reused, document_id,
report}`. `ref` is the file; `render_id` its row in the document's render log;
`reused` is true when an earlier, unchanged file was used. `report` counts
`sections`, `images` and `tables`, and what was left out: `images_skipped`
(a picture that is not PNG, JPEG, GIF or WebP, too large, or only linked),
`bodies_cut` (past 32 MiB of text) and `links_flattened`. In a PDF, a
picture the typesetter could not read is shown as its alt text. A Markdown render
also returns `image_refs`, the pictures written beside it.

## Errors

- You do not hold the Document tool, or a Document hook refuses the export.
- The document or chunk is not there, or not readable to you.
- `to` names the inbox, a read-only volume or one you are not bound to, or a
  file that exists without `overwrite`.
- `format` is `pdf` and the server has no typesetter (Typst) where it
  converts files: render as `docx`, or ask an operator.
- The file would pass 64 MiB, or a PDF 2000 pages: render a part of the
  document with `chunk`.

## Examples

The board minutes as a Word document to send:

```json
{"op": "render", "doc_path": "/board/2026/minutes", "scope": "tenant", "format": "docx"}
```

The same as a PDF to print, on letter paper, with a table of contents:

```json
{"op": "render", "doc_path": "/board/2026/minutes", "scope": "tenant", "format": "pdf", "toc": true, "page_size": "letter"}
```

The same as a web page, with a table of contents:

```json
{"op": "render", "doc_path": "/board/2026/minutes", "scope": "tenant", "format": "html", "toc": true}
```

One section as Markdown, kept in the `reports` volume:

```json
{"op": "render", "document": "4f1c…", "chunk": "9a0e…", "format": "md", "to": "volume:reports:2026/decisions.md"}
```

## House styles

A style is a Document in the tenant's tree. Its root's text is the style's
JSON (or one fenced json block of it), and its first picture is the logo
shown above the title. Every key but `loomcycle_style` is optional:

```json
{"loomcycle_style": 1, "font": "serif", "size": 11, "heading_scale": 1.1,
 "colors": {"text": "#1b1b1b", "heading": "#0f3d6e", "link": "#1d4ed8"},
 "page": {"size": "a4", "margin_mm": 20},
 "header": "Acme Ltd · Internal", "footer": "Confidential",
 "page_numbers": true, "logo_width_mm": 30}
```

- `font`: `sans` or `serif` (Noto Sans or Noto Serif; code stays Noto Sans
  Mono). `size`: 8–14 points. `heading_scale`: 0.8–1.5.
- `colors`: `#rrggbb`. `page.size`: `a4` or `letter` (the call's `page_size`
  wins). `page.margin_mm`: 10–50.
- `header`, `footer`: one line of at most 120 characters, printed on every
  page of a PDF, Word or ODT file and at the top and bottom of an HTML page
  or e-book page. `page_numbers` (default true) ends a paged file's footer
  with the page number. `logo_width_mm`: 10–80.

An unknown key or a value out of range refuses the render and names the key;
a style never falls back to the built-in look. Markdown is never styled. A
style you may not read is refused like one that does not exist.
