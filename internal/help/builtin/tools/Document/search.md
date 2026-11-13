---
name: Document/search
description: "Document op=search — find chunks whose BODIES mean or say what your free-text query does, across every document in one scope."
---
`search` is the way in when you do not know which document or chunk holds
what you need. It matches your text against chunk bodies by meaning and, on a
server with a word index, by the words themselves too, across every document
in the scope, and returns the best chunks with their document ids. With the
word index, a name, a code or an error string the meaning-match misses is
still found by its words; without it (you can tell: every `rank_score` then
equals its `score`), an exact string that is not found may still be there —
look with `query_chunks` and `LIKE`.

Which op to use:

- **`search`** — you have a question or topic in words.
- **`query_chunks`** — you know the structure: a document, a type, a status, a
  tag, a parent; or you need exact matching (SQL).
- **`related`** — you already have a chunk and want others with similar
  content.
- **`backlinks`** — you have a chunk and want what links to it.

One call reads ONE scope. If the answer might be in the tenant store as well
as the user store, search both, in two calls.

## Arguments

- `query` (required) — the text to match, e.g. a question.
- `limit` — at most this many chunks, default 10, capped at 50.
- `scope` — `user` (default), `agent` or `tenant`. See
  `{"op":"help","topic":"scopes"}`.
- `under_path` / `under_paths` — only chunks of documents named at or under
  this path (any of these paths).
- `exclude_under_path` / `exclude_under_paths` — never chunks of documents named
  at or under these paths.
- `document_id` / `document_ids` — only these documents.
- `exclude_document_ids` — never these documents.
- `type` / `types` — only chunks of these types, each including its subtypes.
- `status` / `statuses` — only chunks with exactly one of these statuses.
- `tag` — only chunks carrying exactly this tag.
- `tag_prefix` — only chunks whose tag is this one or nested under it
  (`prefix/…`).
- `concept` — only chunks about this concept of the tenant ontology's concept
  schemes, or about a concept narrower than it. Name it by its chunk id, its
  name, or its tag (`<scheme>/<path>`, e.g. `topics/risk-management`). A name
  two concepts share is refused with their tags; pass one of those.
- `concept_prefix` — the same, for every concept whose tag is this path or lies
  under it, e.g. `topics` for a whole scheme.
- `expand` — `{kinds?, hops?, budget_chars?}`: after ranking, add the chunks
  the top five hits are related to (one hop, in either direction), e.g. the
  article a clause cites or the term it is defined by. `kinds` names the
  relation kinds to follow (each with its narrower kinds); without it the
  expansion follows the tenant's declared walk kinds, or every relation except
  `about` and `mentions` when none are declared. `hops` may only be `1`: for a
  longer walk use `graph_recall`. `budget_chars` (default 2000, at most 8000)
  caps the characters the added rows spend on titles and quotes. Only asserted
  relations in force are followed (at `as_of` when you pass it), and only from
  hits you may read. The filter arguments apply to the added chunks too: an
  expansion never brings back a chunk the filter keeps out. Pass `expand: {}`
  for the defaults.
- `as_of` — only chunks whose stated validity contains this moment (unix
  nanoseconds); a chunk with no stated validity always matches.
- `with_projection` — `true` also returns `query_xyz`: your query placed
  on the scope's 3D map of chunks by meaning, the map `graph_snapshot`'s
  `projection` block draws. Absent when the server keeps no map for the scope.
- `facets` — any of `type`, `folder`, `concept`: also count, beside the hits,
  how many documents among the matches have each value. See "Counting what
  matches" below.
- `facet_window` — with `facets`, how many of the chunks nearest your query the
  counts cover: default 100, at most 500 (a larger value is lowered to 500).
- `with_summary` — add each hit's nearest annotation as `summary` (`{level, brief, abstract?, stale?, chunk_id?}`): the brief on every hit, the abstract on the hits of the first five documents, within `summary_chars` (default 2000).
- `annotations` — `none` (default), `include` or `only`. Rank the
  annotations too: the brief and abstract written for sections, documents and
  folders. `include` fuses them with the passages, one row per target; `only`
  answers from them alone — the shape of a question about the corpus as a
  whole. A row found this way carries `matched_annotation: {level, text}` and
  `summary`; a folder's row is its `_annotation` document, with `folder`.
- `annotation_levels` — with `annotations`, the levels to rank: any of
  `branch`, `document`, `path` (default all).
- `summary_chars` — with `with_summary`, the most abstract characters the answer carries (default 2000, at most 8000).

## Aiming the search

The filter arguments above narrow which chunks the search looks at BEFORE it
ranks anything. That matters: the search keeps only its best few dozen candidates,
so in a scope that holds several kinds of document — board files, norms,
decisions, your own earlier reports — the candidates can all come from the
wrong kind. A filter makes them come from the right one. Filter whenever the
scope holds more than one kind of document; it costs one structural lookup.

Different kinds of filter combine with AND; the values of one list with OR. A
singular argument and its plural twin are one filter (`under_path: "/a"` plus
`under_paths: ["/b"]` means either path). Paths resolve in this scope's own
tree. An unknown path, type, status or tag is not an error: it matches nothing,
and the response says so. A malformed path is.

With a filter, the response also carries `filter`, what it resolved to:

```text
filter: {
  applied: {under_paths, exclude_under_paths, document_ids, exclude_document_ids,
            types, statuses, tag, tag_prefix, concept, concept_prefix,
            concept_matched_by, as_of},   (only what you passed)
  matched_documents: 14,     documents with at least one matching chunk
  matched_chunks: 312,       chunks the search could return
  type_expanded_to: [...],   present when a type was widened to its subtypes
  inverted: false            true when the filter was sent as "everything except"
}
```

When access-control rules keep you from reading part of the scope, the filter
covers only what you may read, and so do the counts.

How a concept matches depends on the scope, and `concept_matched_by` says
which. In the `tenant` scope, where the concepts are, a chunk matches when it
has a `subject` relation (or a narrower kind, such as `primary_topic`) to a
concept of the set; a proposed, rejected, refuted or retired relation does not
count, and a tag alone does not either (`"statement"`). With `as_of`, the
relation must also be in force at that moment: one that had ended by then, or
had not yet begun, does not count; an undated one always does. In a `user` or `agent`
scope a relation cannot reach a concept, so a chunk matches when it carries the
concept's tag or a tag under it (`"tag"`). A concept the ontology does not
declare, or one you may not read, matches nothing.

`matched_chunks: 0` means the filter itself matched nothing — check the path,
type or status — as opposed to an empty result with matches, which means nothing
in that part of the tree is relevant. A call with no filter has no `filter` key.

## Counting what matches

The hits are the best few chunks; `facets` says how wide the material behind
them is — whether the answer sits in one document or is spread over a dozen in
several folders. With `facets: ["type", "folder", "concept"]` (any of them) the
response also carries:

```text
facets: {
  type:    [{value: "rfc", count: 7}, {value: "guide", count: 2}],
  folder:  [{value: "/kb/rfcs", count: 7}, {value: "/kb/guides", count: 2}],
  concept: [{value: "topics/risk-management", count: 3}],
  truncated: ["folder"],   the facets whose list was cut (absent when none)
  facet_window: 100,       the window asked for
  chunks_counted: 87,      chunks the counts cover (only ones you may read)
  documents_counted: 9,    documents those chunks belong to
  complete: false          true: the counts cover every matching chunk
}
```

What is counted: the documents of the `facet_window` chunks nearest your query
by meaning, plus the ranked hits, after every filter argument, and only
documents you may read. Rows that `expand` adds, and rows found through
annotations, are not counted: they are not matches of your query. `facets` is
refused with `annotations: "only"`, which ranks no passage. Counts are of
DOCUMENTS: a document with five matching chunks counts once.

`complete: true` means the window reached the end of what matches, so the
counts cover all of it. `false` means more may lie beyond the window — raise
`facet_window` or narrow with a filter. Read `complete`, not `chunks_counted`:
the two can differ, and on some servers the window is approximate and is never
reported complete.

- `type` — the document's type (its root chunk's type). An untyped document
  counts in `documents_counted` but has no value.
- `folder` — the folder of each Path name the document is filed under: the name
  without its last segment, `/` at the top. A document with two names counts in
  both folders; one filed nowhere has no folder.
- `concept` — the concepts of the tenant ontology the document is about. Only
  the document's chunks in force at `as_of`, or now when you pass none, are
  read: a part an edition closed or a later fact replaced no longer counts. In
  the `tenant` scope such a chunk counts through a `subject` relation (or a
  narrower kind) to the concept — never a proposed, rejected, refuted or retired
  one, and with `as_of` only one in force then (without `as_of` its dates are
  not consulted, as for the `concept` filter). In a `user` or `agent` scope it
  counts through a tag at or under the concept's tag. Each value is a concept's
  tag, the most specific one: a document about
  `topics/risk-management/outsourcing` is not counted again under
  `topics/risk-management`. Empty when the tenant has no ontology or you may not
  read it. When the ontology cannot be read, `concept` is left out and
  `concept_error` says so; the hits and the other facets are still returned.

Each list is most documents first, then by value, at most 50 values. Read the
spread before the hits: many documents in several folders means the question
needs more than the top hits; one document holding every match means read that
one. A search without `facets` has no `facets` key and costs nothing extra.

## Returns

`{chunks: [{chunk_id, score, rank_score, title, type, document_id, path?,
has_body?, valid_at?, invalid_at?}]}`, best match first. `path` is where the
hit's document is filed: its first Path name in sort order, absent when it is
filed nowhere. `has_body: false` marks a section whose own body is empty and
whose text is in its children: `get_chunk` on it lists them. `valid_at` /
`invalid_at` are the chunk's own stated validity, when it has one. `rank_score` is what the search's order used — the meaning and
word matches combined. `score` is the raw strength of the match that first found
the chunk: the meaning similarity, or, for a chunk found by its words alone, a
word-match weight on a different scale. So a chunk found by its words can have a
low `score` and still rank first. How high is "good" depends on the embedding
model, so compare within one result rather than to a fixed number. When the
response says `reranked: true`, the order is the reranker's and `rank_score` no
longer describes it.
With `with_projection` and a map, the response also carries `query_xyz: [x,
y, z]` (4 decimals), on the same map as the `projection` block of
`graph_snapshot`. On a `umap` map the query is placed among its own hits, so
it is absent when no hit has a place yet.
Bodies are not included: read a hit with `{"op":"get_chunk","id":<chunk_id>}`.

With `expand`, the chunks it added follow the ranked hits in the same list,
without scores, each with `expanded_via: {kind, from_chunk_id, quote?}` — the
relation, the hit it came from, and that relation's evidence. The response
then also carries `expanded` (how many) and `expand_truncated` when the budget
cut some. Both count only chunks you may read and the filter allows; the others
are left out before the budget is spent.

The order is the same as `Memory op=search` over the same scope with prefix
`doc.chunk:` on the server's own memory — the two run one search. (An agent
whose memory is served elsewhere searches that elsewhere with `Memory`; its
documents are always here.) That includes the operator's rerank:
when your document searches are reranked by a model, the response carries
`reranked`, and `rerank_reason` when it is false (see `Memory` `search`). A
false is never an error — the order is then the search's own.

A hit may carry `matched_unit: {kind, text}`: the chunk was found through a
short description, claim or question generated for its document (a "derived
search unit") rather than by its own words, and the unit is why it matched. The
chunk appears once however many of its units matched; the units themselves are
never results, and read the chunk for the text.

A chunk with an empty body is never found. An image chunk is found by its
caption (and its description, once one exists).

## Errors

- `search: missing required field: query ...`.
- `search: requires a configured embedder / vector memory` — the server has no
  embedding model, so this op cannot work here; retrying is pointless. Use
  `query_chunks` (filters, or `sql` with `LIKE` on titles) instead.
- `search: embed query: ...` — the embedding service failed; a retry may
  work, and `query_chunks` does not need it.
- `search: filter_too_wide: ...` — the filter matches so much of a very large
  scope that it cannot be sent to the search; the message gives
  `matched_chunks`, `scope_chunks` and the limit. Narrow it with `type`,
  `status` or a tag, or search a smaller subtree.
- `search: concept "…" names N concepts: …` — the name is shared; pass one of
  the listed tags as `concept`.
- `search: concept_too_wide: ...` — the concept covers more concepts than one
  search can be sent; name a narrower one.
- `search: expand.hops must be 1 ...` — expansion is one hop; walk further with
  `graph_recall`.
- `search: expand.budget_chars must be 0..8000`.
- `search: unknown facet "…"` — pass any of `type`, `folder`, `concept`.
- `search: facet_window must be positive` / `search: facet_window is taken with
  facets` — omit `facet_window` for the default, or add `facets`.
- `search: facets count passages and are not taken with annotations "only"` —
  drop `facets`, or use `annotations: "include"`.

## Examples

Find where the rollback procedure is written down:

```json
{"op": "search", "query": "how do we roll back a failed database migration"}
```

```json result
{"chunks": [
  {"chunk_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "score": 0.82, "rank_score": 0.032, "title": "Rollback plan", "document_id": "b4407b522dc11495d3de371311db17f0", "path": "/ops/runbooks/database"},
  {"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "score": 0.61, "rank_score": 0.016, "title": "Migrate the database", "type": "task", "document_id": "b4407b522dc11495d3de371311db17f0", "path": "/ops/runbooks/database"}]}
```

Look only among the regulator's norms in force, and never at earlier gate reports:

```json
{"op": "search", "query": "minimum capital requirement", "scope": "tenant", "under_path": "/qg/norms", "type": "norm.clause", "status": "in_force", "exclude_under_path": "/qg/reports"}
```

```json result
{"chunks": [
  {"chunk_id": "5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b", "score": 0.79, "rank_score": 0.033, "title": "Article 12", "type": "norm.clause", "document_id": "a1b2c3d4e5f60718293a4b5c6d7e8f90"}],
 "filter": {"applied": {"under_paths": ["/qg/norms"], "exclude_under_paths": ["/qg/reports"], "types": ["norm.clause"], "statuses": ["in_force"]},
  "matched_documents": 14, "matched_chunks": 312, "inverted": false}}
```

The same question against the tenant's shared documents:

```json
{"op": "search", "query": "how do we roll back a failed database migration", "scope": "tenant", "limit": 5}
```

The same, each hit with the summary of the section, document or folder it sits
in, so you can tell what a passage belongs to before you open it:

```json
{"op": "search", "query": "how do we roll back a failed database migration", "scope": "tenant", "with_summary": true, "limit": 8}
```

A general question — a policy, a rule, how something is done — matched against
the passages and the summaries together:

```json
{"op": "search", "query": "what is our policy for releasing a version", "scope": "tenant", "annotations": "include", "with_summary": true, "limit": 8}
```

A question about the material as a whole, matched against the summaries of
folders and documents only. To count what lies behind it, run a passage search
with `facets` (below); `facets` is not taken here:

```json
{"op": "search", "query": "what do we have on access control", "scope": "tenant", "annotations": "only", "annotation_levels": ["path", "document"], "limit": 15}
```

Inside one document, to find the exact value near a hit you already have:

```json
{"op": "search", "query": "default retry limit", "scope": "tenant", "document_id": "b4407b522dc11495d3de371311db17f0", "limit": 6}
```

Inside the folders you chose from their summaries:

```json
{"op": "search", "query": "rotating access tokens", "scope": "tenant", "under_paths": ["/kb/security", "/kb/operations"], "with_summary": true, "limit": 8}
```

Only guides about one concept of the tenant ontology:

```json
{"op": "search", "query": "rotating access tokens", "scope": "tenant", "concept": "topics/security", "type": "guide"}
```

With the passages the best hits cite or are defined by, along the tenant's walk
kinds:

```json
{"op": "search", "query": "rotating access tokens", "scope": "tenant", "expand": {"hops": 1}, "limit": 6}
```

A question whose answer depends on what the best clauses cite or implement:
the norms in force on 1 January 2026, about one concept, with the provisions
they rely on added after the ranked hits:

```json
{"op": "search", "query": "who approves an outsourcing contract", "scope": "tenant", "under_path": "/norms", "concept": "topics/risk-management/outsourcing", "as_of": 1767225600000000000, "limit": 5, "expand": {"kinds": ["cites", "implements", "defines"], "budget_chars": 3000}}
```

```json result
{"chunks": [
  {"chunk_id": "5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b", "score": 0.77, "rank_score": 0.031, "title": "Point 14", "type": "norm.clause", "document_id": "a1b2c3d4e5f60718293a4b5c6d7e8f90", "valid_at": 1735689600000000000},
  {"chunk_id": "9c8b7a6f5e4d43c2b1a0f9e8d7c6b5a4", "title": "Article 41", "type": "law.article", "document_id": "0c1d2e3f4a5b4c6d8e9f0a1b2c3d4e5f",
   "expanded_via": {"kind": "implements", "from_chunk_id": "5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b", "quote": "pursuant to Article 41 of the Law"}}],
 "expanded": 1,
 "filter": {"applied": {"under_paths": ["/norms"], "concept": "topics/risk-management/outsourcing", "concept_matched_by": "statement", "as_of": 1767225600000000000},
  "matched_documents": 6, "matched_chunks": 140, "inverted": false}}
```

Read the added chunk with `get_chunk`, and the relation's full evidence with
`get_edge` (find its id with `backlinks` on the added chunk).

How wide is what the tenant holds on outsourcing — which kinds of document, in
which folders, about which concepts:

```json
{"op": "search", "query": "outsourcing of critical functions", "scope": "tenant", "limit": 3, "facets": ["type", "folder", "concept"]}
```

```json result
{"chunks": [
  {"chunk_id": "5e1f0c2a9b8d4e7f8a6b5c4d3e2f1a0b", "score": 0.77, "rank_score": 0.031, "title": "Point 14", "type": "norm.clause", "document_id": "a1b2c3d4e5f60718293a4b5c6d7e8f90", "path": "/norms/outsourcing-regulation"}],
 "facets": {
  "type": [{"value": "norm", "count": 4}, {"value": "board.decision", "count": 3}, {"value": "report", "count": 1}],
  "folder": [{"value": "/norms", "count": 4}, {"value": "/board/2025", "count": 3}, {"value": "/reports", "count": 1}],
  "concept": [{"value": "topics/risk-management/outsourcing", "count": 6}, {"value": "topics/governance", "count": 2}],
  "facet_window": 100, "chunks_counted": 100, "documents_counted": 8, "complete": false}}
```

Eight documents in three folders, and `complete: false`, so there may be more:
an overview of this needs the folders, not the top three
hits. Narrow the count to one folder with `under_path` and a larger window:

```json
{"op": "search", "query": "outsourcing of critical functions", "scope": "tenant", "under_path": "/board", "facets": ["type"], "facet_window": 300}
```
