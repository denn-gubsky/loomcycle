---
name: Agent/cancel
description: "Agent op=cancel — stop a resident sub-agent's current turn; the child stays open for your next send."
---
`cancel` interrupts the turn a resident child is running — one that is taking
too long or heading the wrong way. **The child stays open** and keeps its
conversation; `send` it a corrected instruction next. To shut it down, use
`close` instead. Cancelling a child that is not running a turn does nothing.

## Arguments

- `op` (required) — `cancel`.
- `child_run_id` (required).

## Returns

The child's `{child_run_id, state, output}` after the turn stopped.

## Errors

- `missing required field: child_run_id`.
- `resident sub-agent "r_..." not found ...` — it was already closed.

## Examples

Stop a turn that went off track:

```json
{"op": "cancel", "child_run_id": "r_4e8b1c9a2f7d6035"}
```
