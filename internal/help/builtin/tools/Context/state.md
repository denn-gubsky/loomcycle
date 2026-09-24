---
name: Context/state
description: "Context op=state — read back the structured state you have recorded so far on a stateful run."
---
On a **stateful** run you do not keep a growing conversation; you keep a
structured state that each step updates. `state` returns that state as it
stands now, so you can check what you have already recorded before deciding
the next step. On any other run there is no such state and the call is
refused.

## Arguments

None besides `op`.

## Returns

`{state: {...}}`. On a stateful run that has recorded nothing yet, `state` is
an empty object — not an error.

## Errors

- `structured execution state is not available (this run is not in
  context.mode=stateful)` — this run keeps an ordinary conversation. Use
  `{"op":"self"}` for your context usage instead; retrying is pointless.

## Examples

Read what you have recorded so far:

```json
{"op": "state"}
```

```json result
{"state": {"candidates_checked": 14, "shortlist": ["c_4471", "c_5120"], "next": "rank shortlist"}}
```
