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

- `scope` (required) — `agent`, `user` or `tenant`. One scope per call.
- `query` (required) — plain-language text to match by meaning.
- `top_k` — maximum results (default 10, at most 50).
- `sources` — which kinds of row to return: `facts` (distilled by a
  consolidator), `notes` (written directly by an agent), `documents`
  (Document chunk bodies), `traces` (raw conversation turns). Default: all
  except `traces`. **`traces` must be asked for alone**, and `documents` may
  not be paired with only one of `facts`/`notes`. Allowed sets: `[facts]`,
  `[notes]`, `[facts, notes]`, `[documents]`, `[traces]`, or
  `[facts, notes, documents]`.
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
then the results are NOT restricted to the kinds you asked for.

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

Find what was actually said in conversations during a week, favouring recent turns:

```json
{"op": "search", "scope": "user", "query": "dentist appointment", "sources": ["traces"], "when": {"from": "2026-03-01T00:00:00Z", "to": "2026-03-08T00:00:00Z"}, "rank": {"semantic_weight": 0.8, "recency_weight": 0.2}}
```
