---
name: Document/memberships
description: "Document op=memberships — every place one document holds: its class chain, its concepts in each taxonomy with how each placement was made, the broader concepts those imply, and the folders it is filed under."
---
`memberships` answers "where does this document sit?" in one call. It is the
reverse of walking down a taxonomy: given one document, it lists every view that
holds it.

- **Its class chain.** The document's own type, then each broader class of the
  tenant ontology in force, up to the root (`Report`, `Document`).
- **Its concepts, by taxonomy.** For each concept scheme (taxonomy) that holds
  the document, each concept it is placed under, with the concept's path from
  the top of the scheme and how each placement was made.
- **Its folders.** Each Path name it is filed under, with that name's folder.

What counts as a placement is what `search` with `concept:` reads in the same
scope:

- In the **tenant** scope, a `subject` statement (or a narrower kind, such as
  `primary_topic`) from any chunk of the document to the concept. Each comes
  with its statement id, kind, method, status and confidence.
- In a **user or agent** scope, where no statement reaches the ontology, a
  chunk carrying the concept's tag (`<scheme>/<path>`) or a tag under it.
  These come with `via: "tag"` and the tag.

A document placed under a concept is also a member of every broader concept
above it, as `describe_ontology` `with_counts` counts it. Those broader concepts
are listed too, with `implied: true` and no placement of their own: a document
under `topics/risk/outsourcing` is also in `topics/risk`.

Proposed statements are listed by default, as `get_edges` lists them, with
`status: "proposed"`. Pass `include_proposed: false` to leave them out. A
proposal makes the document a member of nothing yet, so it implies no broader
concept. Rejected, refuted and retired statements are never listed.

It asks read on the document, as `get_document` does. Classes above the
document's own type and the taxonomies are the ontology's text: a caller who
may not read the ontology document gets neither, and an issue says why.

`get_document` with `with_memberships: true` returns this same answer under
`memberships`.

## Arguments

- `id` (or `document_id`, or `path`) — the document. A `path` is resolved to
  the document filed there.
- `scope` — the scope the document is in (default user).
- `include_proposed` — `false` leaves proposed placements out (default true).

## Returns

`{document_id, scope, classes, taxonomies, folders, issues?}`:

- `classes` — the class chain, the document's own type first; `[]` for an
  untyped document.
- `taxonomies` — each scheme that holds the document, in the ontology's order:
  `{scheme, concepts}`. Each concept is `{tag, name, path, chunk_id, implied?,
  placements?}`, sorted by tag, so a broader concept comes before the narrower
  ones under it.
  - `path` — the concept's names from the top of the scheme down to itself.
  - `placements` — each `{statement_id, kind, chunk_id, method, status,
    confidence?}` in the tenant scope, or `{chunk_id, via: "tag", tag}`
    elsewhere. `chunk_id` is the chunk of the document the placement starts
    from.
  - `implied` — true when the document is under this concept only through a
    live placement under a narrower one. It may still carry a proposal.
- `folders` — each `{folder, path}`: a Path name the document is filed under,
  and the folder that name is in. `[]` for a document filed nowhere.
- `issues` — why part of the answer is missing: the ontology is a draft (a
  draft has no taxonomies), the caller may not read it, or it could not be
  read.

## Errors

- A permission error when the caller's access-control rules do not let it read
  the document.
- `memberships: no such document` — the id is not a document of the scope.
- A path that names no document is refused as `get_document` refuses it.

## Examples

```json
{"op": "memberships", "scope": "tenant", "path": "/kb/reports/q3-outsourcing"}
```

```json result
{"document_id": "3f9a1c2b7d4e4f60a1b2c3d4e5f60718", "scope": "tenant", "classes": ["Report", "Document"], "taxonomies": [{"scheme": "topics", "concepts": [{"tag": "topics/risk", "name": "Risk", "path": ["Risk"], "chunk_id": "5e6f708192a3b4c5d6e7f8091a2b3c4d", "implied": true}, {"tag": "topics/risk/outsourcing", "name": "Outsourcing", "path": ["Risk", "Outsourcing"], "chunk_id": "7a1b2c3d4e5f40718293a4b5c6d7e8f9", "placements": [{"statement_id": "c0ffee00112233445566778899aabbcc", "kind": "subject", "chunk_id": "9d8c7b6a5f4e43d2c1b0a99887766554", "method": "manual", "status": "asserted", "confidence": 0.9}]}]}, {"scheme": "rank", "concepts": [{"tag": "rank/internal", "name": "Internal", "path": ["Internal"], "chunk_id": "1a2b3c4d5e6f40718293a4b5c6d7e8f0", "placements": [{"statement_id": "d00d00112233445566778899aabbccdd", "kind": "subject", "chunk_id": "9d8c7b6a5f4e43d2c1b0a99887766554", "method": "classify", "status": "proposed", "confidence": 0.6}]}]}], "folders": [{"folder": "/kb/reports", "path": "/kb/reports/q3-outsourcing"}]}
```
