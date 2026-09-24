---
name: Agent/poll
description: "Agent op=poll — check a resident sub-agent's state and output so far, without giving it new input."
---
`poll` looks at a resident child without sending anything. Use it after a
`send` that returned `state: "running"`, to wait for the turn to finish.

## Arguments

- `op` (required) — `poll`.
- `child_run_id` (required).
- `timeout_ms` — 0 (default) returns a snapshot at once; above 0 waits up to
  that long for the child to finish its turn.

## Returns

`{child_run_id, state, output}` — the output so far. `awaiting_input` means
the turn is done and the child is ready for the next `send`.

## Errors

- `missing required field: child_run_id`.
- `resident sub-agent "r_..." not found ...` — it was closed or timed out.

## Examples

Wait up to a minute for a long turn to finish:

```json
{"op": "poll", "child_run_id": "r_4e8b1c9a2f7d6035", "timeout_ms": 60000}
```

```json result
{"child_run_id": "r_4e8b1c9a2f7d6035", "state": "awaiting_input", "output": "Revenue by region: ..."}
```
