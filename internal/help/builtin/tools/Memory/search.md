---
name: Memory/search
description: "Memory op=search — semantic search over one scope's stored rows (facts, notes, document chunks, or raw conversation turns), with optional time window, key prefix, hybrid ranking and dedup."
---
`search` embeds your `query` and returns the stored rows closest in meaning,
best first. It is the flexible sibling of `recall`: it can also reach
Document chunk bodies and raw conversation turns, filter by key prefix, and
tune the ranking. For "what do I remember about X", `recall` is usually the
simpler call. It needs the operator's embedder and vector store, and it only
finds rows that carry an embedding — a `set` without `embed: true` is not
searchable.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`. One scope per call. A
  `tenant` search reaches the tenant's shared documents only when the agent
  holds `sql_scopes: [tenant]` as well as `memory_scopes: [tenant]` — the same
  two grants the Document tool needs for them. Without the second grant, asking
  for documents is refused, and a search that does not ask leaves them out.
  To search another scope's documents in the same call, see
  `documents_scope`.
- `query` (required) — plain-language text to match by meaning.
- `top_k` — maximum results (default 10, at most 50).
- `sources` — which kinds of row to return: `facts` (distilled by a
  consolidator), `notes` (written directly by an agent), `documents`
  (Document chunk bodies), `traces` (raw conversation turns). Default: all
  except `traces`. **`traces` must be asked for alone**, and `documents` may
  not be paired with only one of `facts`/`notes`. Allowed sets: `[facts]`,
  `[notes]`, `[facts, notes]`, `[documents]`, `[traces]`, or
  `[facts, notes, documents]`.
- `documents_scope` — `agent`, `user` or `tenant`: also search the documents
  of this scope, ranked together with this call's own results. See
  "Searching a shared corpus" below.
- `prefix` — only rows whose key starts with this.
- `when` — narrow by when the thing was SAID: `from` / `to` (RFC3339),
  `slack` (how far outside still counts, default `"3d"`), `missing`
  (`"prefer"`, the default, keeps undated rows but ranks them lower;
  `"require"` drops them). `as_of` instead asks what was TRUE at that
  instant. Give a generous window — a remark usually comes a day or more
  after the event.
- `rank` — hybrid ranking weights: `semantic_weight`, `recency_weight`,
  `recency_half_life_hours`. Omit for pure semantic ranking.
  (`source_weight` and `frequency_weight` are accepted but contribute
  nothing yet.)
- `dedup` — `{enabled: true, threshold, mode}` collapses near-duplicates
  (cosine ≥ `threshold`, default 0.92; `mode` `drop` (default), `merge` or
  `keep`).

## Returns

`{entries, query_embedding_dim, truncated}`. Each entry has `key`, `kind`
(`fact`, `note`, `document` or `trace`), `value`, `score` (raw cosine
similarity), `rank_score` (what the order used), `embedded_with`
`{provider, model}` and `expires_at`. A `document` hit adds `chunk_id` and,
when known, `document` and `title`; a `trace` hit adds `session_id`,
`source_run_id`, `speaker` and `said_at` where recorded.

Extra keys appear only when relevant: `time_filter` (when you passed `when`),
`dedup_dropped` (when dedup was on), `rank_note`, `source_turns` and
`source_turns_found` (conversation turns the operator has you attach), and
`sources_applied: false` with a `note` when this backend ignored `sources` —
then the results are NOT restricted to the kinds you asked for. With a
`documents_scope` different from `scope`, every entry also carries `scope`:
the scope it came from.

A document hit may carry `matched_unit: {kind, text}`: the chunk was found
through a short description, claim or question the operator generated for it
(a "derived search unit"), and the unit is why it matched. The chunk appears
once however many of its units matched. `sources: [notes]` or `[facts]` never
returns units, and neither does any search when the operator switched them off
for this agent.

`reranked` appears when the operator has this agent's document searches
reranked by a model: `true` means the order is the reranker's (so `rank_score`
no longer describes it), and `false` comes with a `rerank_reason` —
`not_configured`, `not_a_document_search` (your `sources` excluded
`documents`), `too_few_candidates`, `timeout`, `call_failed`, `unparseable` or
`not_supported_by_memory_backend`. A `false` is never an error: the results are
the search's own order. You cannot request or switch off the rerank.

## Searching a shared corpus

Your own memory and the documents you consult often live in different scopes:
your notes in `user`, the organisation's reference documents in `tenant`.
`documents_scope` searches both in ONE call. The call searches `scope` as it
always does (with your `sources`, `prefix` and `when`), and also searches the
documents, and only the documents, of `documents_scope`. Both sets of
results are ranked together into one list and cut by one `top_k`, so you do
not merge two lists by hand. Each entry then says which scope it came from in
`scope`. Use it to tell what you remember from what the documents say.

- It takes only documents from the other scope: facts and notes still come
  from `scope` alone.
- It needs what searching that scope's documents with the Document tool
  needs, and that scope in `memory_scopes` too. For `tenant` that is
  `memory_scopes: [tenant]` and `sql_scopes: [tenant]`. Without it the whole
  call is refused, never answered from one scope.
- `documents_scope` equal to `scope` is the same as adding `documents` to
  `sources`, and the answer is the one that call would give.
- It cannot be combined with `sources: [traces]`.

## Aiming at documents

When the scope holds more than one kind of document and only part of them
should answer, aim the search with `under_paths`, `exclude_under_paths`,
`document_ids` and `exclude_document_ids` (lists; a path means a document named
at or under it, in this scope's own Path tree). They narrow **document chunks
only** — facts and notes the same search returns are not touched — and they
are applied before ranking, so the best document matches come from the part of
the tree you named rather than being crowded out by the rest.

They need a search that can return documents: no `sources`, or `sources` that
include `documents`. On a `[facts]`, `[notes]` or `[traces]` search, or a
`prefix` outside the document namespace, they are refused rather than ignored.
They work in every scope, each in that scope's own tree; on `tenant` they need
both tenant grants (`memory_scopes` and `sql_scopes`), as reading tenant
documents does. With a `documents_scope` different from `scope`, they aim at
that scope's documents and resolve in its tree. The documents of `scope` are
then not filtered (leave `documents` out of `sources` if you do not want
them).

The answer then carries `filter: {applied, matched_documents, matched_chunks,
inverted}` — what the filter resolved to. `matched_documents: 0` means the path
names no document here (check the spelling or the scope), not that nothing
relevant was found. For chunk-level filters — type, status, tags, a validity
instant — use the Document tool's `search`.

## Errors

- `search: missing required field: query`.
- `memory: vector index not configured ...` / `memory: no embedder
  configured ...` — semantic search is not available on this server.
  Retrying is pointless; use `get` / `list` instead.
- `memory: query embedding dimension does not match stored rows ...` — the
  operator changed embedder models; only an operator can fix it.
- `memory: sources=[traces] must be asked for on its own ...` or `this
  combination of sources cannot be filtered in one query ...` — pick one of
  the allowed sets above.
- `search: memory: none of the requested sources were recognised ...` —
  check the spelling: `facts`, `notes`, `documents`, `traces`.
- `search: under_paths narrows the documents a search returns, and sources [facts]
  cannot return documents` — add `documents` to `sources`, or drop the filter.
- `search: documents not configured on this server ...` — this server has no
  documents to aim at; search without the document filter.
- `search: document filter: Document: scope=tenant is not granted to this agent
  ...` — a permission refusal: reading this scope's documents needs a grant the
  agent lacks (the message names it). Drop the filter, or ask an operator to
  grant it.
- `search: reading tenant documents through Memory needs sql_scopes: [tenant] as
  well as memory_scopes: [tenant] ...` — a permission refusal: this search asked
  for the tenant's documents (`documents` in `sources`, a `doc.chunk:` prefix, or
  a document filter) and the agent lacks the second grant. Search facts and notes
  only, or ask an operator to grant it.
- `search: unknown documents_scope "org" (want one of: agent, user, tenant)`.
- `search: documents_scope cannot be combined with sources [traces] ...` —
  search conversation turns in a call of their own.
- `search: documents_scope: Memory tool: scope "tenant" not in this agent's
  memory_scopes ...` or `search: documents_scope: Document: scope=tenant is not
  granted to this agent ...` — a permission refusal: the agent lacks a grant
  for that scope's documents (the message names it). Search without
  `documents_scope`, or ask an operator to grant it.
- `search: documents are not configured on this server, so documents_scope
  cannot be searched` / `search: documents_scope applies only to this
  server's own memory ...` — this server cannot run the second search. Search
  without `documents_scope`, or make a second call with `scope` set to the
  documents' scope.
- `search: when.from "October" is not an RFC3339 timestamp` — resolve dates
  yourself, e.g. `2025-10-01T00:00:00Z`.

## Examples

Find what this user said about travel plans:

```json
{"op": "search", "scope": "user", "query": "upcoming travel plans", "top_k": 5}
```

```json result
{"entries": [{"key": "notes/boston-trip", "kind": "note", "value": "Met the design team in Boston",
  "score": 0.81, "rank_score": 0.81, "embedded_with": {"provider": "openai", "model": "text-embedding-3-small"},
  "expires_at": null}], "query_embedding_dim": 1536, "truncated": false}
```

Search only the tenant's shared documents:

```json
{"op": "search", "scope": "tenant", "query": "refund policy for annual plans", "sources": ["documents"]}
```

Search this user's own notes together with the tenant's reference documents,
in one ranked list:

```json
{"op": "search", "scope": "user", "documents_scope": "tenant", "query": "supplier audit frequency", "sources": ["notes"], "under_paths": ["/norms"]}
```

```json result
{"entries": [{"key": "doc.chunk:9b1e…", "kind": "document", "scope": "tenant", "chunk_id": "9b1e…",
  "document": "Quality norms", "title": "Supplier audits", "value": "Suppliers are audited every 12 months …",
  "score": 0.74, "rank_score": 0.031}, {"key": "notes/audit-followup", "kind": "note", "scope": "user",
  "value": "Asked for the 2026 audit schedule", "score": 0.69, "rank_score": 0.029}],
  "filter": {"scope": "tenant", "applied": {"under_paths": ["/norms"]}, "matched_documents": 3, "matched_chunks": 41, "inverted": false},
  "query_embedding_dim": 1536, "truncated": false}
```

Search everything this user knows about a decision, but keep the gates' own
reports out of the documents it can return:

```json
{"op": "search", "scope": "user", "query": "approval of protocol 86", "exclude_under_paths": ["/qg/reports"]}
```

Find what was actually said in conversations during a week, favouring recent turns:

```json
{"op": "search", "scope": "user", "query": "dentist appointment", "sources": ["traces"], "when": {"from": "2026-03-01T00:00:00Z", "to": "2026-03-08T00:00:00Z"}, "rank": {"semantic_weight": 0.8, "recency_weight": 0.2}}
```
