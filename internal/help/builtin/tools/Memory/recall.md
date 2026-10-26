---
name: Memory/recall
description: "Memory op=recall — find what you remember about something, by meaning: remembered facts and notes in one scope, each with its score, kind, and the original wording it came from."
---
`recall` is the everyday way to ask "what do I know about X". Pass a
plain-language `query`; you get back remembered facts and notes, best match
first. Each item is something REMEMBERED, not something established: a
`note` is a remark an agent recorded; only a `fact` was distilled by a
consolidator. It needs the operator's embedder and vector store. It reads
**one scope** — a `user` recall does not see `tenant` knowledge, so make a
second call when you need both. The exception is documents:
`documents_scope` adds another scope's documents to the same recall.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`. Recalling the tenant's
  shared documents (`documents` in `sources`) needs `sql_scopes: [tenant]` as
  well as `memory_scopes: [tenant]`, the two grants the Document tool needs.
- `query` (required) — what you want to know, in plain language.
- `top_k` — maximum results (default 10, at most 50).
- `threshold` — a 0..1 score floor; weaker matches are dropped. Default 0
  (no floor).
- `sources` — default `[facts, notes]`. Add `documents` to include Document
  prose (`[facts, notes, documents]`), or ask for `[traces]` alone to get raw
  conversation turns. See `Memory/search` for the allowed sets.
- `documents_scope` — `agent`, `user` or `tenant`: also recall the documents
  of this scope, ranked together with this scope's facts and notes into one
  list. See "Searching a shared corpus" below.
- `when` — narrow by when things were said, or `as_of` for what was true at
  an instant. Same shape as on `search`; give a generous window.
- `include_source` — default `true`: attach the verbatim span each fact was
  distilled from. Pass `false` for a smaller result.
- `include_turns` — default `false`: also attach the whole conversation turn
  each fact came from. Use it when the question turns on WHEN or exact
  wording; it makes the result much larger.

## Returns

`{memories: [...]}`. Each memory has `id` (the fact's stable key), `memory`
(the text), `score`, and when known `kind` (`fact`, `note`, …), `source` (the
original wording), `source_session_id` and `source_run_id` (where it was said
— History can open that chat), `observed_at`, `valid_at`, `metadata`, and
with `include_turns` a `turn: {text, speaker}`. With a `documents_scope`
different from `scope`, each memory also carries `scope`: the scope it came
from.

A remark often carries the time it was SAID. When it says "yesterday", the
event happened relative to that time, not at it.

Extra keys: `turns_attached` (and `turns_dropped_for_budget`) when turns were
requested; `source_turns` and `source_turns_found` when the operator has you
attach matching conversation turns; `time_filter` when you passed `when`;
`sources_applied: false` with a `note` when the backend ignored `sources`;
`matched_unit: {kind, text}` on a document found through one of its derived
search units (see `search`); `reranked` (and `rerank_reason` when false) when the operator has your
document searches reranked — see `search`. A recall reaches the rerank only
when `sources` includes `documents`.

An empty `memories` list is not an error. It can also mean the fact was
`add`ed recently and is still queued for consolidation.

## Searching a shared corpus

When your memory is in one scope and the reference documents are in another
(your notes in `user`, the organisation's documents in `tenant`), set
`documents_scope` to the documents' scope. The recall returns this scope's
facts and notes (or whatever `sources` names) together with that scope's
documents, ranked as one list under one `top_k`. Each memory says which
scope it came from, so you can tell what you remember from what the
documents say.

It needs what searching that scope's documents with the Document tool needs,
and that scope in `memory_scopes` too (for `tenant`: `memory_scopes: [tenant]`
and `sql_scopes: [tenant]`). Without it the whole call is refused.
`documents_scope` equal to `scope` adds `documents` to the recall's `sources`.
It cannot be combined with `sources: [traces]`. The errors are the ones listed
on `Memory/search`.

## Aiming at documents

A recall that includes `documents` in `sources` can be aimed at part of the
scope's documents with `under_paths`, `exclude_under_paths`, `document_ids`
and `exclude_document_ids`, exactly as on `Memory/search`: they narrow document
chunks only, before ranking, and the answer carries the same `filter` block.
A recall without `sources` returns facts and notes only, so a document filter
on it is refused — ask for `[facts, notes, documents]` or `[documents]`. A path
is a name in ONE scope's tree: when you recall in two scopes, each call
resolves the path in its own. With a `documents_scope`, they aim at that
scope's documents instead, and the recall may then leave out `sources`. Like
on `search`, they work in every scope; on `tenant` they need both tenant
grants. For chunk-level filters use the
Document tool's `search`.

## Errors

- `recall: missing required field: query`.
- `recall: memory: vector index not configured ...` / `recall: memory: no
  embedder configured ...` — semantic recall is unavailable on this server.
  Retrying is pointless; fall back to `get` / `list` with known keys.
- `memory: add/recall require a memory-layer-capable backend ...` — this
  agent's backend cannot recall.
- `recall: memory: sources=[traces] must be asked for on its own ...` — split
  into two calls.
- `recall: exclude_under_paths narrows the documents a search returns, and a
  recall without sources (facts and notes) cannot return documents` — add
  `sources: ["facts", "notes", "documents"]`.
- `recall: document filter: Document: scope=tenant is not granted to this agent
  ...` — a permission refusal: reading this scope's documents needs a grant the
  agent lacks (the message names it). Drop the filter, or ask an operator to
  grant it.
- `recall: reading tenant documents through Memory needs sql_scopes: [tenant]
  as well as memory_scopes: [tenant] ...` — a permission refusal: the recall
  asked for the tenant's documents and the agent lacks the second grant. Recall
  facts and notes only, or ask an operator to grant it.
- `recall: when.from ... is not an RFC3339 timestamp` — resolve the date
  yourself.

## Examples

What do I know about this user's diet?

```json
{"op": "recall", "scope": "user", "query": "dietary preferences and allergies"}
```

```json result
{"memories": [{"id": "memory/preference/diet", "memory": "Is vegetarian", "score": 0.78,
  "kind": "fact", "source": "[2026-03-02] I'm vegetarian now.",
  "source_session_id": "b4407b522dc11495d3de371311db17f0", "observed_at": "2026-03-02T18:04:00Z"}]}
```

The same question against knowledge shared across the tenant — a separate call:

```json
{"op": "recall", "scope": "tenant", "query": "office catering dietary options", "top_k": 5}
```

What this user remembers about supplier audits, together with what the
tenant's quality norms say:

```json
{"op": "recall", "scope": "user", "documents_scope": "tenant", "query": "supplier audit frequency", "under_paths": ["/norms"]}
```

Recall facts, notes and documents, but not the reports filed under `/qg/reports`:

```json
{"op": "recall", "scope": "user", "query": "what was decided about supplier audits", "sources": ["facts", "notes", "documents"], "exclude_under_paths": ["/qg/reports"]}
```

A question about WHEN, with the originating turns attached:

```json
{"op": "recall", "scope": "user", "query": "when did they move to Lisbon", "include_turns": true, "when": {"from": "2026-01-01T00:00:00Z", "to": "2026-04-01T00:00:00Z"}}
```
