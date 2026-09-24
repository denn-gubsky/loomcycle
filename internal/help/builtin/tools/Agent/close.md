---
name: Agent/close
description: "Agent op=close — shut a resident sub-agent down and free what it holds; safe to call twice."
---
`close` ends a resident child and frees its resources, such as a sandbox
container. **Close every child you open** once you are done with it: an open
child counts against the run's limit on resident sub-agents until it is
closed or its idle time runs out. Closing an already-closed child is fine.

## Arguments

- `op` (required) — `close`.
- `child_run_id` (required).

## Returns

`{child_run_id, state: "closed", output: ""}`. Read the child's last output
before you close it (the previous `send` or `poll` returned it).

## Errors

- `missing required field: child_run_id`.

## Examples

```json
{"op": "close", "child_run_id": "r_4e8b1c9a2f7d6035"}
```
