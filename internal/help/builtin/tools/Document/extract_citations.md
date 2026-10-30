---
name: Document/extract_citations
description: "Document op=extract_citations — find the citations in a document's text with its class's citation grammar, and resolve each to the cited work and part. Read-only."
---
`extract_citations` applies the citation grammar of a document's class to its
text and returns every citation it finds, each resolved to the cited work and
part. It is read-only: it never files a placeholder for a work the scope does
not hold. Do that with `resolve_anchor` and `create_placeholder: true`.

The grammar is the `@citation_grammar` the document's class declares or
inherits, under a confirmed ontology. A grammar's `resolve:` line fills in each
citation:
- `work_key=` is the cited work's key;
- `anchor=` is the cited part;
- `kind=` (a literal or a `{group}`) is the relation the citation states, e.g.
  `amends`.

Each citation resolves in the document's own scope, as of `as_of`. Without
`as_of`, it resolves as of the citing part's own edition date, so a 2019 edition
citing "Article 12" means the Article 12 in force in 2019.
A work's first edition records no date for its parts, so its citations resolve
as of now. Pass `as_of` (e.g. the work's publication date) when that matters.

## Arguments

- `document_id` — every part of a document, in document order; or
- `id` — one chunk. Pass exactly one of the two.
- `as_of` — the moment to read and resolve at (unix nanoseconds). With
  `document_id`, only the parts in force then are read.
- `limit` — the most citations to return (default 200, at most 1000).
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{document_id, grammar, grammar_issue?, hits, truncated?}`:

- `grammar` — `{name, class}`, or `null` when the class declares no citation
  grammar (then `hits` is `[]`).
- `grammar_issue` — the class names a grammar that cannot be used, and why.
- `hits` — each `{chunk_id, quote, char_start, char_end, groups?, work_key?,
  anchor?, kind?, resolved}`:
  - `char_start`/`char_end` are character offsets into the chunk's text, the
    unit statement evidence uses.
  - `groups` holds the pattern's named groups that matched.
  - `resolved` is `{document_id?, chunk_id?, exact, placeholder}`.
    `exact` is true when the cited part was found; with only `document_id`, the
    citation reached the work but not the part. With neither, the work is not one
    you can read here: either the scope does not hold it or you may not read the
    document that does.
- `truncated` — more citations matched than `limit`.

## Errors

- `extract_citations: pass exactly one of document_id … or id …`.
- `no such document` / `no such chunk` in this scope.
- A permission error when the caller may not read the document or chunk.

## Examples

```json
{"op": "extract_citations", "document_id": "b4407b522dc11495d3de371311db17f0", "scope": "tenant"}
```

```json result
{"document_id": "b4407b522dc11495d3de371311db17f0", "grammar": {"name": "law-ua", "class": "Law"}, "hits": [{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "quote": "Закону України № 2297-VI", "char_start": 41, "char_end": 65, "groups": {"number": "2297-VI"}, "work_key": "law:2297-VI", "kind": "amends", "resolved": {"document_id": "0c1d2e3f4a5b4c6d8e9f0a1b2c3d4e5f", "exact": false, "placeholder": false}}]}
```
