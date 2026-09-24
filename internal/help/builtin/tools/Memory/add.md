---
name: Memory/add
description: "Memory op=add — hand over conversation turns to be distilled into durable facts by a background consolidator; asynchronous, so a later recall may not see them yet."
---
`add` gives the memory layer some conversation to learn from. By default it
**queues** the turns and returns `status: "pending"`; a scheduled
consolidator later reads the queue and writes the durable facts it finds.
**Do not expect to `recall` them in the same run** — that is what `set` is
for. Use `add` when you want facts extracted from what was said, and `set`
when you already know the exact value to store.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`: whose memory the facts
  belong to. A user's own statements usually go to `user`.
- `messages` (required) — a non-empty array of `{role, content}`, where
  `role` is `user`, `assistant` or `system` and `content` is non-empty text.
- `infer` — `true` (default) queues the turns for fact extraction. `false`
  stores them verbatim as ONE row instead, immediately, and embeds it when the
  server can, so `recall` can find it right away.
- `metadata` — string-to-string context attached to the ingestion, e.g.
  `{"source": "support-chat"}`.

## Returns

`{status, event_id}`. `status` is `"pending"` (queued for consolidation) or
`"done"` (stored now — the `infer: false` path). `event_id` is the queued
item's id, or the stored row's key.

## Errors

- `add: missing required field: messages (a non-empty array of {role, content})`.
- `add: messages[2] has empty content` — drop that turn or fill it.
- `add: messages content (N bytes) exceeds max M bytes` — send fewer turns.
- `memory: add/recall require a memory-layer-capable backend ...` — this
  agent's memory backend does not support `add`. Retrying is pointless; use
  `set`.

## Examples

Queue what the user just told you for consolidation:

```json
{"op": "add", "scope": "user", "messages": [{"role": "user", "content": "I moved to Lisbon last month and I'm vegetarian now."}, {"role": "assistant", "content": "Noted — Lisbon, and vegetarian."}]}
```

```json result
{"status": "pending", "event_id": "pend_9c1e4a7b2f3d8e60"}
```

Store a note verbatim so it is recallable at once:

```json
{"op": "add", "scope": "agent", "messages": [{"role": "assistant", "content": "The staging database is reset every Sunday at 02:00 UTC."}], "infer": false, "metadata": {"source": "ops-runbook"}}
```

```json result
{"status": "done", "event_id": "mem_4b7f0d2a9e1c6853"}
```
