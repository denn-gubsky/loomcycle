---
name: TeamDef/poll
description: "TeamDef op=poll — read the team walks you ran in poll mode: their states, and each ended walk's answer, optionally waiting for one or all of them."
---
`poll` reads the walks you started with `run` in `mode: "poll"`, without
starting anything. Name them with `run_ids`, or name none to get every such
walk whose answer you have not read yet — so a series of bare polls hands you
each answer once. Polling an ended walk by id returns its answer every time.

## Arguments

- `run_ids` — walks you ran in poll mode, by the `run_id` each returned.
- `wait` — `none` (default) answers at once; `any` waits until one of the
  named walks that is still running ends; `all` waits until all of them have.
- `wait_ms` — the longest `wait` blocks, in ms. Absent = the runtime's cap
  (60000 unless the operator set another); a larger value is cut to it. A
  runtime pause ends the wait early, with what is known so far: poll again
  after it.

## Returns

`{"walks": [{run_id, name, state, ...}], "pending": N}`. `state` is `running`,
`held` (a person is holding it: a breakpoint or cap question, or a member held
for review), `completed`, `failed` or `cancelled`. An ended walk's row also
carries what a waited-for `run` answers — `def_id`, `status`, `final_state`,
`final_output`, `steps` — and `error` when it did not complete. A walk stopped
by an iteration cap is `failed` with `status: "iteration_cap"`, `capped_state`
and the steps so far. `pending` counts the walks still running or held.

The walks in one answer share a quarter of your context window. A walk whose
text does not fit its share is cut — `final_output` first keeps up to the
whole share, the `steps` outputs split what is left — and says
`truncated: true`. Its whole answer stays on its run (`run_id`); poll it alone
to give it the whole quarter.

## Errors

- `poll: not a team walk of this run: ...` — an id you did not get from a
  poll-mode `run` (a sub-agent's id included: read those with `Agent` poll).
- `poll: this run has no team walks to poll` — you are not in an agent's run.
- `poll: unknown wait ...` — `none`, `any` or `all`.

## Examples

Wait up to two minutes for a walk you started:

```json
{"op": "poll", "run_ids": ["r_6f2a9c1e4b8d7035"], "wait": "all", "wait_ms": 120000}
```

```json result
{"walks": [{"run_id": "r_6f2a9c1e4b8d7035", "name": "pr-review", "state": "completed",
  "def_id": "tdf_9a1c3e5b7d2f4086", "status": "completed", "final_state": "done",
  "final_output": "Approve with two nits: ...",
  "steps": [{"state": "review", "agent": "reviewer", "edge": "success", "next": "done", "output": "Approve with two nits: ..."}]}],
 "pending": 0}
```

Read whatever walks ended since you last looked:

```json
{"op": "poll"}
```
