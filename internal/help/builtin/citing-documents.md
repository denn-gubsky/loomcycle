---
name: citing-documents
description: "Citing a document or a chunk in your answer as a loom: link — [title](loom:doc/<document_id>) or [title](loom:chunk/<chunk_id>), with ?scope=tenant|agent outside the user scope — so a client can open it. Ids copied from Document output, never invented."
aliases: [Document/citing, citing, cite, citations-in-answers, loom-links]
---
When your answer mentions a document, or a part of one, link it so the reader's
client can preview and open exactly that document or section. A title alone
cannot be opened: titles repeat, and a client has no way to know which of two
"Release plan" documents you meant. The ids can be opened, and you already have
them from every Document operation.

## The format

A Markdown link with the title as its text and a `loom:` address:

- `[Release plan](loom:doc/<document_id>)` for a whole document.
- `[Timeline](loom:chunk/<chunk_id>)` for a section, a passage or a fact.
- Add `?scope=tenant` (or `?scope=agent`) when the id was read in that scope:
  `[Pricing policy](loom:doc/<document_id>?scope=tenant)`. Leave it out for the
  user scope, which is the default. An id resolves only in its own scope, so a
  link without the right scope opens nothing.

A document is cited as `doc`, never as `chunk/<root_chunk_id>`: the root chunk
is the document. Document ids and chunk ids look alike (32 hex characters), so
the `doc` / `chunk` word is what tells a client which one it is.

## The rules

- **Copy the ids exactly** from Document output: a document's `document_id`,
  a chunk's `id` (a `search` hit's `chunk_id`). Never make an id up, shorten it
  or guess it. When you are not sure of an id, name the document by its title
  without a link.
- **Put the link where you mention the document**, in the sentence, not in a
  list of ids at the end.
- **Answers only.** Inside a document body, link with the body grammar instead:
  `[[Title]]` or `[[/path/to/doc]]`. That one is resolved when the body is
  written and becomes a `references` relation; a `loom:` link in a body is
  only text.

## Example answer

> The beta starts after the security review, per the
> [Timeline](loom:chunk/5d8e2f1a0b9c4d3e7f6a5b4c3d2e1f00) of the
> [Release plan](loom:doc/b4407b522dc11495d3de371311db17f0). Pricing for the
> beta follows the [Pricing policy](loom:doc/0e3c9a1f7b2d4c6e8a5f1d3b9c7e2a40?scope=tenant).

Here the first two ids came from a `search` in the user scope (a hit's
`chunk_id`, and its `document_id`), the third from `get_document` in the
tenant scope.
