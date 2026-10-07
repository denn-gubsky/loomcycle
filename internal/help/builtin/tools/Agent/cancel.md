---
name: Agent/cancel
description: "Agent op=cancel — end background children you started, or stop a resident sub-agent's current turn (the child stays open for your next send)."
---
`cancel` has two forms.

**Background children** (started with `mode: "poll"`, team walks you ran in
poll mode included): pass `child_run_ids`. Each is cancelled and its run ends
`cancelled` — a walk's members with it; cancelling one child of a
`parallel_spawn` leaves the others running. A child that already finished is
left as it ended. A resident child named here has its current turn stopped,
as below.

**One resident child**: pass `child_run_id` to interrupt the turn it is
running — one that is taking too long or heading the wrong way. **The child
stays open** and keeps its conversation; `send` it a corrected instruction
next. To shut it down, use `close` instead. Cancelling a child that is not
running a turn does nothing.

## Arguments

- `op` (required) — `cancel`.
- `child_run_ids` — background or resident children of this run, team walks
  you ran in poll mode included (by their `run_id`).
- `child_run_id` — instead: one resident child.

## Returns

Background form: `{"children": [{child_run_id, agent, kind?, index?, state}]}` with
each child's state once the cancel has taken effect — `cancelled`, or how it
ended if it finished first.

Resident form: the child's `{child_run_id, state, output}` after the turn
stopped.

## Errors

- `not a child of this run: ...` — an id you did not start; nothing was
  cancelled.
- `missing required field: child_run_id` — pass `child_run_ids` or
  `child_run_id`.
- `resident sub-agent "r_..." was closed ...` / `was reaped by the runtime
  (...)` — it was already ended; there is no turn to stop. A resident child
  whose run ended on its own answers as `poll` does, with how it ended.

## Examples

Stop one child of a batch you no longer need:

```json
{"op": "cancel", "child_run_ids": ["r_8d3f6a1b0c9e2754"]}
```

```json result
{"children": [{"child_run_id": "r_8d3f6a1b0c9e2754", "agent": "researcher", "index": 1, "state": "cancelled"}]}
```

Stop a resident child's turn that went off track:

```json
{"op": "cancel", "child_run_id": "r_4e8b1c9a2f7d6035"}
```
