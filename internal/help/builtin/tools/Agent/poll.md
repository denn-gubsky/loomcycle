---
name: Agent/poll
description: "Agent op=poll — read your background children's states and results, optionally waiting for one or all of them; or check one resident sub-agent without giving it new input."
---
`poll` reads your children without starting anything. It has two forms.

**Background children** (started with `mode: "poll"` — sub-agents, and team
walks you ran with `TeamDef op=run` in poll mode): name them with
`child_run_ids`, or a whole `parallel_spawn` with `batch_id`, or name none to
get every background child whose result you have not read yet — so a series
of bare polls hands you each result once. Polling a finished child by id
returns its result every time you ask.

**One resident child** (opened with `open`): pass `child_run_id` to see its
current turn, as after an `open` or `send` that returned
`state: "running"`.

## Arguments

- `op` (required) — `poll`.
- `child_run_ids` — background or resident children of this run, by the id
  `spawn`, `parallel_spawn` or `open` returned, or the `run_id` of a team walk
  you ran in poll mode.
- `batch_id` — every child of one `parallel_spawn` started with
  `mode: "poll"`. Not with `child_run_ids`.
- `wait` — `none` (default) answers at once; `any` waits until one of the
  named background children that is still running ends; `all` waits until all
  of them have. A resident child is reported as it is and never waited for.
- `wait_ms` — the longest `wait` blocks, in ms. Absent = the runtime's cap
  (60000 unless the operator set another); a larger value is cut to it. A
  runtime pause ends the wait early, with what is known so far: poll again
  after it.
- `child_run_id` — instead of the above: one resident child.
- `timeout_ms` — with `child_run_id` only: 0 (default) returns a snapshot at
  once; above 0 waits up to that long for the child to finish its turn.

## Returns

Background form: `{"children": [{child_run_id, agent, kind?, index?, state,
output?, error?, structured?, status?, truncated?, structured_omitted?,
structured_bytes?}], "pending": N}`. `state` is
`queued` (waiting for a slot), `running`, `held` (held for a review verdict),
`completed`, `failed`, `cancelled` or `timeout`; a resident child is `idle`
when it is waiting for your next `send`. A finished child carries its
`output` (or `error`), and `structured` when it keeps structured state;
`status` is `"timeout"` for one its `timeout_ms` stopped, and
`"max_iterations"` for one that stopped at its iteration limit before it
finished — its `state` is `failed`, `error` says so, and its last answer,
which may be incomplete, is still in `output`. A team walk's row
has `kind: "team"`, `agent: "team:<name>"`, and its final output as `output`;
`TeamDef op=poll` reads its whole answer (final state, steps). `pending` counts the
children in the answer that are still queued, running or held. The rows share
a quarter of your context window: an `error` or `output` is cut to its share,
and a `structured` state that does not fit is left out whole, replaced by
`structured_omitted: true` and its size in `structured_bytes`. A cut row
carries `truncated: true` and the whole answer stays in its run's transcript.

Resident form: `{child_run_id, state, output}` — the output so far.
`awaiting_input` means the turn is done and the child is ready for the next
`send`. A child whose run has ended still answers for up to an hour afterwards:
`completed` or `failed` with its last turn's output, or the iteration-limit
error with its last answer.

## Errors

- `not a child of this run: ...` — an id or batch you did not start (or that
  does not exist). Use the ids your own calls returned.
- `pass child_run_ids or batch_id, not both`.
- `timeout_ms bounds a resident child's poll ...` — with `child_run_ids` use
  `wait` and `wait_ms`.
- `resident sub-agent "r_..." was closed ...` / `was reaped by the runtime
  (...)` — it was ended for you; `open` a new one.
- `resident sub-agent "r_..." not found ...` — an id you never opened, or a
  child that ended more than an hour ago.

## Examples

Wait up to 30 seconds for every child of a batch:

```json
{"op": "poll", "batch_id": "fan_3c9e1a7b5d2f8064", "wait": "all", "wait_ms": 30000}
```

```json result
{"children": [
  {"child_run_id": "r_51c0e7a9d2b84f36", "agent": "researcher", "index": 0, "state": "completed", "output": "[sub-agent agent_id=a_... run_id=r_...]\n..."},
  {"child_run_id": "r_8d3f6a1b0c9e2754", "agent": "researcher", "index": 1, "state": "running"}],
 "pending": 1}
```

Read whatever finished since you last looked:

```json
{"op": "poll"}
```

Wait up to a minute for a resident child's long turn to finish:

```json
{"op": "poll", "child_run_id": "r_4e8b1c9a2f7d6035", "timeout_ms": 60000}
```

```json result
{"child_run_id": "r_4e8b1c9a2f7d6035", "state": "awaiting_input", "output": "Revenue by region: ..."}
```
