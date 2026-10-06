---
name: TeamDef/cancel
description: "TeamDef op=cancel — end team walks you ran in poll mode before they finish; their members are cancelled with them."
---
`cancel` ends walks you started with `run` in `mode: "poll"`. Each walk is
cancelled: the member runs it has going are cancelled with it, no further
state starts, and its run ends `cancelled` with the reason
`cancelled by its parent`. The call waits (up to 15 seconds) for each walk to
stop, then answers with how each ended. A walk that had already finished is
left as it ended; `poll` still reads its answer.

Only walks of your own run can be cancelled here. A walk you ran in
`mode: "detach"` is not your child, and cannot be.

## Arguments

- `run_ids` (required) — the walks to end, by the `run_id` each poll-mode `run`
  returned.

## Returns

`{"walks": [{run_id, name, state, error?}]}` — `state` is `cancelled` once the
cancel has taken effect, or how the walk ended if it finished first
(`completed`, `failed`). `error` says why a walk did not complete.

## Errors

- `cancel: not a team walk of this run: ... — nothing was cancelled` — an id
  you did not get from a poll-mode `run` (a sub-agent's id included: cancel
  those with `Agent` cancel); no walk was cancelled.
- `cancel: missing required field: run_ids` — name the walks to end.

## Examples

End a walk you no longer need:

```json
{"op": "cancel", "run_ids": ["r_6f2a9c1e4b8d7035"]}
```

```json result
{"walks": [{"run_id": "r_6f2a9c1e4b8d7035", "name": "pr-review", "state": "cancelled",
  "error": "run: the walk was cancelled: cancelled by api: cancelled by its parent"}]}
```
