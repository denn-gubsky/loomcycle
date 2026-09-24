---
name: Memory/supersede
description: "Memory op=supersede — retire a stored fact without deleting it, optionally naming the fact that replaces it; consolidation machinery, needs the consolidation grant and the target's lease."
---
`supersede` is consolidation machinery: a background consolidation agent
uses it to retire a fact that a newer one corrects. **An ordinary agent
rarely needs it** — to change your own entry, `set` it again; to remove it,
`delete`. A superseded fact is kept for audit but disappears from `get`,
`list`, `search` and `recall`, and from the fact graph. It needs the
consolidation grant, and you must hold the target's lease (`cursor_lease`)
first.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`: where the fact lives.
- `key` (required) — the fact's key, as `recall` reports it in `id`.
- `superseded_by` — the key of the fact that REPLACES it, when this is a
  correction. Omit when the fact was merely moved (rewritten under the same
  key elsewhere). A key that names nothing is ignored; the retirement still
  happens.

## Returns

`{ok: true}`. When the fact is also stored in the fact graph, the result adds
`chunk_id` and `retired_at` (Unix nanoseconds), plus `superseded_by` when a replacement was
recorded, or `already: true` and `superseded_by_chunk` when an earlier call
had already retired it. A key that matches no fact also returns `{ok: true}`.

## Errors

- `memory: supersede requires the memory_consolidation grant ...` — this
  agent is not a consolidator. Retrying is pointless.
- `supersede: not lease owner — take this target's lease with cursor_lease
  ...` — call `cursor_lease` first, and stop if it reports `acquired: false`.
- `supersede: missing required field: key`.
- `supersede: "..." is hidden from recall but still current in the fact graph:
  ...` — only half the retirement happened. Retry the same call; both halves
  are safe to repeat.

## Examples

Retire an outdated city, recording what replaced it:

```json
{"op": "supersede", "scope": "user", "key": "memory/fact/home-city-porto", "superseded_by": "memory/fact/home-city-lisbon"}
```

```json result
{"ok": true, "chunk_id": "0b6f2c1e-4d7a-4f1b-9c3e-8a2d5e7f1b40", "retired_at": 1773479523000000000, "superseded_by": "memory/fact/home-city-lisbon"}
```

Retire a fact that was rewritten in another scope, with no replacement asserted:

```json
{"op": "supersede", "scope": "user", "key": "memory/fact/checkout-api-owner"}
```
