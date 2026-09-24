---
name: Memory/pending_ack
description: "Memory op=pending_ack — mark drained queue items as consolidated so they are never returned again; consolidation machinery, needs the consolidation grant and the target's lease."
---
`pending_ack` is consolidation machinery: after the facts from drained items
are safely written, ack those items so `pending_drain` never returns them
again. **Ack only after the facts are stored** — an acked item is gone from
the queue for good, and if you ack first and then fail, what it said is lost.
You must hold the target's lease (`cursor_lease`). An ordinary agent does not
need it. Needs the consolidation grant.

## Arguments

- `scope` (required) — the same queue you drained, usually `user`.
- `ids` (required) — the `id` values from `pending_drain`. Ids from another
  scope or unknown ids are silently skipped; acking twice is harmless.

## Returns

`{ok: true, acked}` — `acked` is how many ids you passed.

## Errors

- `pending_ack: not lease owner — take this target's lease with cursor_lease
  ...` — take the lease first; if `acquired` is `false`, stop without acking.
- `pending_ack: missing required field: ids`.
- `memory: pending_ack requires the memory_consolidation grant ...`.
- `pending_ack: cannot verify the target's lease: ...` — a storage fault;
  retry later rather than assuming the lease.

## Examples

Mark two consolidated items done:

```json
{"op": "pending_ack", "scope": "user", "ids": ["pend_9c1e4a7b2f3d8e60", "pend_1a2b3c4d5e6f7081"]}
```

```json result
{"ok": true, "acked": 2}
```
