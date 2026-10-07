---
name: Agent/send
description: "Agent op=send — give a resident sub-agent its next instruction and wait for that turn's output."
---
`send` steers a child you started with `open`. The child sees its whole prior
conversation, so the instruction can build on what it already did. By default
`send` waits for the turn to finish.

## Arguments

- `op` (required) — `send`.
- `child_run_id` (required) — the id `open` returned.
- `prompt` (required) — the next instruction.
- `timeout_ms` — 0 (default) waits until the turn ends. Above 0, a turn still
  going after that long returns `state: "running"` with the output so far;
  then `poll` to wait for it, or `cancel` to stop it.

## Returns

`{child_run_id, state, output}`. `state` is `awaiting_input` when the turn
finished and the child is ready for the next `send`.

## Errors

- `missing required field: child_run_id (the id op=open returned)` / `prompt`.
- `resident sub-agent "r_..." has ended ...` — its run is over and it takes
  no more sends; `poll` it to read its last answer, then `open` a new one.
- `resident sub-agent "r_..." was closed ...` / `was reaped by the runtime
  (...)` — it was ended for you; `open` a new one.
- `resident sub-agent "r_..." not found ...` — an id that is not a resident
  sub-agent your run may address.
- `resident sub-agent "r_..." is not held by this replica, and there is no
  route to the one running it ...` — the child runs on another replica this
  runtime cannot reach; `poll` still reads it.
- `resident sub-agent "r_..." is still running its previous turn ...` — `poll`
  it to wait, or `cancel` the turn, before you `send` again.
- `sub-agent "X" stopped at its iteration limit of N before it finished, so
  its last answer may be incomplete (run r_...). Its last answer: ...` — the
  turn used the last iterations its agent's `max_iterations` allows, and the
  child has ended with it. Its answer follows the message; check it before
  relying on it. To go on, `open` a new one.

## Examples

Ask the next question, waiting at most 30 seconds:

```json
{"op": "send", "child_run_id": "r_4e8b1c9a2f7d6035", "prompt": "Now chart monthly revenue by region.", "timeout_ms": 30000}
```

```json result
{"child_run_id": "r_4e8b1c9a2f7d6035", "state": "running", "output": "Grouping by region..."}
```
