---
name: Memory/cursor_get
description: "Memory op=cursor_get — read a memory target's consolidation watermark and lease; consolidation machinery, needs the consolidation grant."
---
`cursor_get` is consolidation machinery. A background consolidation agent
keeps, per memory target, a **watermark** (the last chat it has consolidated)
and a **lease** (which pass is working the target now). `cursor_get` reads
both without changing anything. **An ordinary agent does not need it**, and
without the consolidation grant every call is refused.

## Arguments

- `scope` (required) — the target: usually `user` (this run's end-user),
  or `agent` / `tenant`.

## Returns

`{watermark_completed_at, watermark_session_id, leased_by,
lease_expires_at}`. A target that was never consolidated has null
timestamps and empty strings — that is normal, not an error.

## Errors

- `memory: cursor_get requires the memory_consolidation grant (add
  memory_consolidation: true to the agent config)` — this agent is not a
  consolidator. Retrying is pointless.
- `cursor_get: ...` wraps a storage failure; retrying is reasonable.

## Examples

Where did consolidation of this user's chats get to?

```json
{"op": "cursor_get", "scope": "user"}
```

```json result
{"watermark_completed_at": "2026-03-13T21:40:12.51Z", "watermark_session_id": "b4407b522dc11495d3de371311db17f0",
 "leased_by": "", "lease_expires_at": null}
```
