---
name: Document/place
description: "Document op=place — where a set of documents, or the top hits of a query, sit together: per taxonomy and for the class tree, the lowest node that covers most of them, with its brief and the share."
---
`place` answers "what are these about, together?" for a handful of documents —
usually the hits of a search. For each taxonomy (concept scheme) of the tenant
ontology it names the LOWEST concept that covers at least `min_share` of the
documents, and for the class tree the lowest class. "Six of the eight hits are
under topics › risk › outsourcing, and all eight are reports" is the answer to
an overview question that a list of hits does not give.

`memberships` is the same question for one document; `place` asks it of a set.

A document counts under a concept when `search` with `concept:` would return
it: a live placement on the concept or on a concept below it (in the `tenant`
scope a `subject` statement, elsewhere a tag). Proposals count nowhere. Lowest
means deepest: a concept with a longer path wins over its broader concept; ties
go to the node covering more documents, then the name.

## Arguments

- `document_ids` — the documents to place, at most 50. Or:
- `query` — place the documents of the top hits of a `search` for this text
  (each document once, in rank order). Pass exactly one of `document_ids`
  and `query`.
- `limit` — with `query`, how many hits: default 10, at most 50.
- `min_share` — the share of the documents a node must cover, default `0.6`,
  above 0 and at most 1. Not strict: 0.6 of five documents is three.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

```
{documents: [document_id, …],
 min_share: 0.6,
 class?: {level: "class", name, chunk_id?, brief?, count, share},
 taxonomies: [{scheme, members, share,
               node?: {level: "concept"|"scheme", name, tag?, path?, chunk_id, brief?, count, share}}, …],
 not_found?: [id, …],
 issues?: […]}
```

- `documents` — the set placed: the ids you passed that exist and you may read,
  or the query's hit documents.
- `taxonomies` — each taxonomy holding at least one of the documents, in the
  ontology's order. `members` is how many of the set it holds anywhere.
  `node` is the lowest concept covering `min_share` of the set; when no concept
  does but the taxonomy as a whole does, the scheme itself (`level: "scheme"`);
  absent when not even that — the set is spread across views, so say so.
- `class` — the lowest class covering `min_share` of the set; absent when none.
- `brief` — the node's annotation, when one has been written.
- `not_found` — ids that name no document, and ids of documents you may not
  read: the two are not told apart.

Every count is over documents you may read. Classes above a document's own
type, the taxonomies and node ids are the ontology's text: a caller who may not
read the ontology document gets the documents' own types and no taxonomy, and an
issue says why. Briefs need the `tenant` scope.

To read the members of a node, search with `concept` set to its tag.

## Errors

- `place: pass document_ids or query, not both`.
- `place: min_share must be above 0 and at most 1`.
- `place: at most 50 documents`.

## Examples

Where the top hits of an overview question sit:

```json
{"op": "place", "scope": "tenant", "query": "outsourcing of critical functions", "limit": 10}
```

```json result
{"documents": ["a1b2c3d4e5f60718293a4b5c6d7e8f90", "b4407b522dc11495d3de371311db17f0"], "min_share": 0.6, "class": {"level": "class", "name": "Report", "chunk_id": "9e1c3a7d2b5f4e6081a2c3d4e5f60718", "count": 2, "share": 1}, "taxonomies": [{"scheme": "topics", "members": 2, "share": 1, "node": {"level": "concept", "name": "Outsourcing", "tag": "topics/risk/outsourcing", "path": ["Risk", "Outsourcing"], "chunk_id": "2a7c9e0b1d3f4a5b6c7d8e9f0a1b2c3d", "brief": "Outsourced services and their oversight.", "count": 2, "share": 1}}]}
```

Where a set of documents you hold sits, naming a node only when all of them are under it:

```json
{"op": "place", "scope": "tenant", "document_ids": ["a1b2c3d4e5f60718293a4b5c6d7e8f90", "b4407b522dc11495d3de371311db17f0"], "min_share": 1}
```
