---
name: History/related
description: "History op=related — chats similar in meaning to a given chat or a free-text query. Needs an embedder; only chats that have been recapped, renamed or annotated can match."
---
`related` finds chats close in meaning to a source: either a chat you name
with `session_id` (its title, summary and description are the source, and it
is left out of its own results) or a free-text `query`. Results are ranked by
similarity and limited to the scope you name. **Only chats that have been
recapped, renamed or annotated are in the similarity index** — older chats
that never were cannot match until one of those is done to them.

## Arguments

- `session_id` or `query` (one is required) — the source. If you pass both,
  `session_id` wins.
- `scope` (pass it) — `user`, `self`, `tenant` or `global`. Omitted: `user`
  when granted, else `self`.
- `limit` — how many similar chats to return (default 10, at most 500).
- `include_archived` — `true` includes archived chats.
- `include_internal` — `true` includes chats served by the runtime's own
  maintenance agents.

## Returns

`{scope, related: [...], count}`, plus `source_session_id` when the source was
a chat or `query` when it was text. Each related chat has the same fields as a
`list` row plus `score`, highest first.

## Errors

- `history: related requires an embedder` — none is configured here. Not
  retryable; use `search` instead.
- `history: related requires session_id or query`.
- `history: related: chat has no title, summary, or description to match on
  (recap or annotate it first)` — run `History/recap` on the source chat, then
  retry.
- `history: chat "<id>" not found` — the source chat is not in THIS scope.
- An empty `related` list is not an error: nothing indexed in this scope is
  close, or the neighbours have never been recapped.

## Examples

Chats about the same thing as one you already have:

```json
{"op": "related", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "limit": 5}
```

```json result
{"scope": "user", "source_session_id": "b4407b522dc11495d3de371311db17f0", "count": 1,
 "related": [{"session_id": "91c3e0a7d24b4f6e8a1b5c7d9e0f2a3b", "agent": "chat", "title": "Release checklist",
              "score": 0.78, "run_count": 1, "status": "completed"}]}
```

Chats about a topic, described in plain words:

```json
{"op": "related", "scope": "user", "query": "rescheduling the product launch"}
```
