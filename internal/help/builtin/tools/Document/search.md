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
- `expand` — `{kinds?, budget_chars?}`: after ranking, add the chunks the top
  five hits are related to (one hop), e.g. the article a clause cites or the
  term it is defined by. Without `kinds` it follows the tenant's declared walk
  kinds, or every relation except `about` and `mentions`. Budget default 2000
  characters (titles and quotes). The filter arguments apply to the added
  chunks too: an expansion never brings back a chunk the filter keeps out.
- `as_of` — only chunks whose stated validity contains this moment (unix
  nanoseconds); a chunk with no stated validity always matches.

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
            types, statuses, tag, tag_prefix, as_of},   (only what you passed)
  matched_documents: 14,     documents with at least one matching chunk
  matched_chunks: 312,       chunks the search could return
  type_expanded_to: [...],   present when a type was widened to its subtypes
  inverted: false            true when the filter was sent as "everything except"
}
```

When access-control rules keep you from reading part of the scope, the filter
covers only what you may read, and so do the counts.

`matched_chunks: 0` means the filter itself matched nothing — check the path,
type or status — as opposed to an empty result with matches, which means nothing
in that part of the tree is relevant. A call with no filter has no `filter` key.

## Returns

`{chunks: [{chunk_id, score, rank_score, title, type, document_id}]}`, best
match first. `rank_score` is what the search's order used — the meaning and
word matches combined. `score` is the raw strength of the match that first found
the chunk: the meaning similarity, or, for a chunk found by its words alone, a
word-match weight on a different scale. So a chunk found by its words can have a
low `score` and still rank first. How high is "good" depends on the embedding
model, so compare within one result rather than to a fixed number. When the
response says `reranked: true`, the order is the reranker's and `rank_score` no
longer describes it.
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

## Examples

Find where the rollback procedure is written down:

```json
{"op": "search", "query": "how do we roll back a failed database migration"}
```

```json result
{"chunks": [
  {"chunk_id": "c7d2e4f6a8b04c1d9e3f5a7b9c1d3e5f", "score": 0.82, "rank_score": 0.032, "title": "Rollback plan", "document_id": "b4407b522dc11495d3de371311db17f0"},
  {"chunk_id": "9a3e7c1f2b4d4a6e8c0f1a2b3c4d5e6f", "score": 0.61, "rank_score": 0.016, "title": "Migrate the database", "type": "task", "document_id": "b4407b522dc11495d3de371311db17f0"}]}
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
