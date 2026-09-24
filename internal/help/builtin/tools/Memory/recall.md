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
second call when you need both.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `query` (required) — what you want to know, in plain language.
- `top_k` — maximum results (default 10, at most 50).
- `threshold` — a 0..1 score floor; weaker matches are dropped. Default 0
  (no floor).
- `sources` — default `[facts, notes]`. Add `documents` to include Document
  prose (`[facts, notes, documents]`), or ask for `[traces]` alone to get raw
  conversation turns. See `Memory/search` for the allowed sets.
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
with `include_turns` a `turn: {text, speaker}`.

A remark often carries the time it was SAID. When it says "yesterday", the
event happened relative to that time, not at it.

Extra keys: `turns_attached` (and `turns_dropped_for_budget`) when turns were
requested; `source_turns` and `source_turns_found` when the operator has you
attach matching conversation turns; `time_filter` when you passed `when`;
`sources_applied: false` with a `note` when the backend ignored `sources`.

An empty `memories` list is not an error. It can also mean the fact was
`add`ed recently and is still queued for consolidation.

## Errors

- `recall: missing required field: query`.
- `recall: memory: vector index not configured ...` / `recall: memory: no
  embedder configured ...` — semantic recall is unavailable on this server.
  Retrying is pointless; fall back to `get` / `list` with known keys.
- `memory: add/recall require a memory-layer-capable backend ...` — this
  agent's backend cannot recall.
- `recall: memory: sources=[traces] must be asked for on its own ...` — split
  into two calls.
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

A question about WHEN, with the originating turns attached:

```json
{"op": "recall", "scope": "user", "query": "when did they move to Lisbon", "include_turns": true, "when": {"from": "2026-01-01T00:00:00Z", "to": "2026-04-01T00:00:00Z"}}
```
