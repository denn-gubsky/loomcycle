---
name: Memory/cursor_advance
description: "Memory op=cursor_advance — move a memory target's consolidation watermark forward to a chat you have consolidated, using the pair cursor_scan returned; consolidation machinery."
---
`cursor_advance` is consolidation machinery: after a pass has consolidated a
page of chats, it moves the target's watermark to the LAST chat of that page,
so the next `cursor_scan` starts after it. The watermark only moves forward
and cannot be reset, so the server checks the pair: it must be a real,
finished chat of this target whose finish time matches. **Copy `session_id`
and `completed_at` verbatim from one `cursor_scan` row.** You must hold the
lease (`cursor_lease`). An ordinary agent does not need it. Needs the
consolidation grant.

## Arguments

- `scope` (required) — the target, the same scope you scanned (usually
  `user`).
- `completed_at` (required) — RFC3339 time, copied from the scan row.
- `session_id` (required) — the chat id from the same scan row.

## Returns

`{ok: true}`. Advancing to a point at or before the current watermark is a
harmless no-op.

## Errors

- `cursor_advance: completed_at does not match chat "..." (it settled at ...)
  — copy the pair verbatim from a cursor_scan row` — you reformatted or mixed
  up the time.
- `cursor_advance: no chat "..." in this tenant` / `does not belong to this
  memory target` — the id is wrong; take it from `cursor_scan`.
- `cursor_advance: chat "..." has not finished yet` — wait; never advance past
  a live chat.
- `cursor_advance: completed_at is in the future ...`.
- `cursor_advance: missing required field: session_id — ...` — the pair
  travels together.
- `cursor_advance: memory cursor advance: not lease owner ...` — take the
  lease with `cursor_lease` first; if it says `acquired: false`, stop.
- `memory: cursor_advance requires the memory_consolidation grant ...`.

## Examples

Mark the page ending at this chat as consolidated:

```json
{"op": "cursor_advance", "scope": "user", "completed_at": "2026-03-14T08:02:55.100231Z", "session_id": "e91c2d7a0f4b4c38a6d15f2b9e7c3a10"}
```

```json result
{"ok": true}
```
