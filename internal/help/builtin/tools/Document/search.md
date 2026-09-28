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

The order is the same as `Memory op=search` over the same scope with prefix
`doc.chunk:` on the server's own memory — the two run one search. (An agent
whose memory is served elsewhere searches that elsewhere with `Memory`; its
documents are always here.) That includes the operator's rerank:
when your document searches are reranked by a model, the response carries
`reranked`, and `rerank_reason` when it is false (see `Memory` `search`). A
false is never an error — the order is then the search's own.

A chunk with an empty body is never found. An image chunk is found by its
caption (and its description, once one exists).

## Errors

- `search: missing required field: query ...`.
- `search: requires a configured embedder / vector memory` — the server has no
  embedding model, so this op cannot work here; retrying is pointless. Use
  `query_chunks` (filters, or `sql` with `LIKE` on titles) instead.
- `search: embed query: ...` — the embedding service failed; a retry may
  work, and `query_chunks` does not need it.

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

The same question against the tenant's shared documents:

```json
{"op": "search", "query": "how do we roll back a failed database migration", "scope": "tenant", "limit": 5}
```
