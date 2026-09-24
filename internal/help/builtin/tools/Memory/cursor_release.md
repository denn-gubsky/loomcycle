---
name: Memory/cursor_release
description: "Memory op=cursor_release — give back the consolidation lease on a memory target when a pass is done; consolidation machinery."
---
`cursor_release` is consolidation machinery: at the end of a pass, release
the target's lease so the next pass can start without waiting for it to
expire. The watermark is not touched. Releasing a lease you do not hold
(already released, expired, or someone else's) is a harmless no-op. An
ordinary agent does not need it. Needs the consolidation grant.

## Arguments

- `scope` (required) — the target you leased, usually `user`.

## Returns

`{ok: true}`.

## Errors

- `memory: cursor_release requires the memory_consolidation grant ...` — not
  a consolidator; retrying is pointless.
- `cursor_release: no stable run/agent identity to own the lease`.
- `cursor_release: ...` wraps a storage failure; retrying is reasonable.

## Examples

Finish a pass on this user's memory:

```json
{"op": "cursor_release", "scope": "user"}
```

```json result
{"ok": true}
```
