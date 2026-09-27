---
name: Document/search
description: "Document op=search — find chunks whose BODIES mean or say what your free-text query does, across every document in one scope."
---
`search` is the way in when you do not know which document or chunk holds
what you need. It matches your text against chunk bodies both by meaning and
by the words themselves, across every document in the scope, and returns the
best chunks with their document ids. A name, a code or an error string that
the meaning-match misses is still found by its words.

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
match first. `rank_score` is what the order used — the meaning and word matches
combined. `score` is the meaning similarity alone: a chunk found by its words
can have a low `score` and still rank first. How high is "good" depends on the
embedding model, so compare within one result rather than to a fixed number.
Bodies are not included: read a hit with `{"op":"get_chunk","id":<chunk_id>}`.

The order is the same as `Memory op=search` over the same scope with prefix
`doc.chunk:` — the two run one search.

A chunk with an empty body is never found. An image chunk is found by its
caption (and its description, once one exists).

## Errors

- `search: missing required field: query ...`.
- `search: requires a configured embedder / vector memory` — the server has no
  embedding model, so this op cannot work here; retrying is pointless. Use
  `query_chunks` (filters, or `sql` with `LIKE` on titles) instead.
- `search: embed: ...` — the embedding service failed; a retry may work.

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
