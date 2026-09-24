---
name: Memory/pending_drain
description: "Memory op=pending_drain — read the queued items (from add, or from context compaction) waiting to be consolidated into facts, oldest first; consolidation machinery."
---
`pending_drain` is consolidation machinery. `add` (and context compaction)
put conversation on a queue; a consolidation pass reads that queue with
`pending_drain`, writes the facts it finds with `set`, then marks the items
done with `pending_ack`. Draining does **not** remove anything — until you
ack, the same items come back next time. **An ordinary agent does not need
it.** Needs the consolidation grant.

## Arguments

- `scope` (required) — the target queue, usually `user`.
- `limit` — items per call (default 50, at most 200).

## Returns

`{pending: [{id, payload, origin, source_session_id, source_run_id,
created_at}]}`, oldest first. `payload` holds the queued `messages` (and any
`metadata`). `origin` is what queued it (`agent_explicit` for `add`, or
`compaction`). Pass an item's `id` as `from_pending` on the `set` that stores
its fact, so the fact records where it came from.

## Errors

- `memory: pending_drain requires the memory_consolidation grant ...` — not a
  consolidator; retrying is pointless.
- `pending_drain: ...` wraps a storage failure; retrying is reasonable.

An empty `pending` list means the queue is empty.

## Examples

Read the next batch for this user:

```json
{"op": "pending_drain", "scope": "user", "limit": 20}
```

```json result
{"pending": [{"id": "pend_9c1e4a7b2f3d8e60", "origin": "agent_explicit",
  "payload": {"messages": [{"role": "user", "content": "I moved to Lisbon last month."}]},
  "source_session_id": "b4407b522dc11495d3de371311db17f0", "source_run_id": "run_7f3a9c",
  "created_at": "2026-03-14T08:01:10.004Z"}]}
```
