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
- `idle_ttl_seconds` — close the child after this long unused: no `send`,
  `poll` or `cancel`, and no turn running (0 = the operator's default).
- `timeout_ms` — 0 (default) waits until the first turn ends. Above 0, a first
  turn still going after that long returns `state: "running"` with the output
  so far and the `child_run_id`; then `poll` to wait for it, or `cancel` to
  stop it.
- `compaction` — override the child's context compaction (it inherits yours),
  with the same fields as on `spawn`. Rarely needed.

## Returns

`{child_run_id, state, output}` — keep `child_run_id` for every later call.
`state` is normally `awaiting_input` after the first turn, or `running` when
`timeout_ms` ran out first.

## Errors

- `missing required field: name` / `prompt`.
- `unknown sub-agent "X" ...` — not a registered name; check
  `Context {"op":"agents"}`.
- `resident sub-agent cap reached ...` — too many open children; `close` one
  first.
- `this run has N children alive and may have at most M at once ...` — the
  run's limit on live children of every kind; close a resident child or wait
  for spawned ones to finish.
- `sub-agent "X" stopped at its iteration limit of N before it finished, so
  its last answer may be incomplete (run r_...). Its last answer: ...` — the
  turn used the last iterations its agent's `max_iterations` allows, and the
  child has ended with it. Its answer follows the message; check it before
  relying on it. To go on, `open` a new one.

## Examples

Open an analyst that keeps a dataset loaded, with a ten-minute idle limit:

```json
{"op": "open", "name": "data-analyst", "prompt": "Load sales_2026.csv from the data volume and describe its columns.", "idle_ttl_seconds": 600}
```

```json result
{"child_run_id": "r_4e8b1c9a2f7d6035", "state": "awaiting_input", "output": "The file has 12 columns: ..."}
```

Start a long first task without waiting for it, and collect it later with
`poll`:

```json
{"op": "open", "name": "data-analyst", "prompt": "Profile every column of sales_2026.csv.", "timeout_ms": 5000}
```

```json result
{"child_run_id": "r_7a1d04c3e9b25f86", "state": "running", "output": "Loading the file..."}
```
