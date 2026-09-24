---
name: Agent/open
description: "Agent op=open — start a resident sub-agent that keeps its conversation and resources between instructions; returns its child_run_id."
---
`open` starts a sub-agent that **stays alive**: it runs its first turn on your
`prompt`, then waits for your next `send`, remembering the whole conversation
and anything it holds (a warm sandbox, a loaded dataset). Use it when a
multi-step job would lose that state if you re-spawned. **You own its
lifecycle: `close` it when you are done.**

## Arguments

- `op` (required) — `open`.
- `name` (required) — a registered agent name.
- `prompt` (required) — the first instruction.
- `def_id` — run a specific version of that agent; its name must match `name`.
- `idle_ttl_seconds` — close the child after this long with no `send`
  (0 = the operator's default).

## Returns

`{child_run_id, state, output}` — keep `child_run_id` for every later call.
`state` is normally `awaiting_input` after the first turn.

## Errors

- `missing required field: name` / `prompt`.
- `unknown sub-agent "X" ...` — not a registered name; check
  `Context {"op":"agents"}`.
- `resident sub-agent cap reached ...` — too many open children; `close` one
  first.

## Examples

Open an analyst that keeps a dataset loaded, with a ten-minute idle limit:

```json
{"op": "open", "name": "data-analyst", "prompt": "Load sales_2026.csv from the data volume and describe its columns.", "idle_ttl_seconds": 600}
```

```json result
{"child_run_id": "r_4e8b1c9a2f7d6035", "state": "awaiting_input", "output": "The file has 12 columns: ..."}
```
