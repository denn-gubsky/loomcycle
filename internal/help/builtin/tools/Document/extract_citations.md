---
name: Document/extract_citations
description: "Document op=extract_citations — find the citations in a document's text with its class's citation grammar, and resolve each to the cited work and part. Read-only."
---
`extract_citations` applies the citation grammar of a document's class to its
text and returns every citation it finds, each resolved to the cited work and
part. It is read-only: it never files a placeholder for a work the scope does
not hold. Do that with `resolve_anchor` and `create_placeholder: true`.

The grammar is the `@citation_grammar` the document's class declares or
inherits, under a confirmed ontology. A class may name several grammars
(`@citation_grammar nbu-resolution, law-ua`): each is applied, and where two
match the same text the earlier match wins. A grammar's `resolve:` line fills in each
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
  grammar (then `hits` is `[]`). With several grammars `name` lists them,
  comma-separated.
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

## From a citation to a statement

A hit is not yet a relation: nothing is written until you file it. For each
hit whose `resolved.chunk_id` is set (or `resolved.document_id`, citing the
work as a whole — then use that document's `root_chunk_id`), file it with
`propose_statement`: `from_id` is the hit's `chunk_id`, `to_id` the resolved
chunk, `kind` the hit's `kind` (or `cites` when it has none), and the
evidence is the hit itself — `{chunk_id, quote, char_start, char_end}` copied
from the hit, so the quote is found where it was read. A hit that resolved to
nothing names a work the scope does not hold: `resolve_anchor` with its
`work_key` and `create_placeholder: true` files a placeholder to point at, or
leave it until the work is imported. Check `get_edges` (or `backlinks` on the
target) first: proposing a relation that is already proposed only adds
evidence, and one already asserted is refused with `statement_asserted`.

For the first hit of the example below, once the cited article is held:

```json
{"op": "propose_statement", "scope": "tenant", "from_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "to_id": "0d1e2f3a4b5c4d6e8f9a0b1c2d3e4f5a", "kind": "amends",
 "evidence": [{"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "quote": "Закону України № 2297-VI", "char_start": 41, "char_end": 65}]}
```

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

One clause, resolved against the cited work as it stood on 1 January 2025:

```json
{"op": "extract_citations", "id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "as_of": 1735689600000000000, "scope": "tenant"}
```
