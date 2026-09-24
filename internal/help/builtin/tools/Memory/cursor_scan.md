---
name: Memory/cursor_scan
description: "Memory op=cursor_scan — list the finished chats a memory target has not consolidated yet, oldest first, after its watermark; consolidation machinery."
---
`cursor_scan` is consolidation machinery: it tells a consolidation pass which
chats to read next. It returns finished chats strictly after the target's
watermark, **oldest first**, so consolidating a page and then advancing to its
LAST row never skips a chat. Chats still running, the pass's own chats, and
the runtime's internal agents' chats are left out. **An ordinary agent does
not need it.** Needs the consolidation grant.

## Arguments

- `scope` (required) — use `user`: the chats belong to this run's end-user.
  Under `agent` the scan matches nothing.
- `limit` — chats per page (default 10, at most 50). Keep pages small
  enough to finish: a pass that runs out of steps before advancing re-reads
  the same page next time.

## Returns

`{sessions: [{session_id, completed_at}], truncated}`. `truncated: true`
means more chats are waiting after this page. **Copy `session_id` and
`completed_at` verbatim** into `cursor_advance` — do not reformat the time.

## Errors

- `memory: cursor_scan requires the memory_consolidation grant ...` — not a
  consolidator; retrying is pointless.
- `cursor_scan: ...` wraps a storage failure; retrying is reasonable.

An empty `sessions` list means there is nothing new to consolidate.

## Examples

The next page of work for this user:

```json
{"op": "cursor_scan", "scope": "user"}
```

```json result
{"sessions": [
  {"session_id": "b4407b522dc11495d3de371311db17f0", "completed_at": "2026-03-13T21:40:12.513942Z"},
  {"session_id": "e91c2d7a0f4b4c38a6d15f2b9e7c3a10", "completed_at": "2026-03-14T08:02:55.100231Z"}],
 "truncated": false}
```

A smaller page for a slow extractor:

```json
{"op": "cursor_scan", "scope": "user", "limit": 3}
```
