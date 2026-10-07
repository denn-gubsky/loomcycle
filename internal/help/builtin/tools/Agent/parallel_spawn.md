---
name: Agent/parallel_spawn
description: "Agent op=parallel_spawn — run several sub-agents concurrently and get every result back in one envelope, one entry per child; or start them in the background with mode poll."
---
`parallel_spawn` fans a batch of independent tasks out to sub-agents that run
at the same time, and returns when **all** of them have finished. One child
failing does not fail the call: **check `ok` on each entry** and retry or skip
only the ones that failed.

## Arguments

- `op` (required) — `parallel_spawn`.
- `spawns` (required) — 1 to 32 entries, each `{name, prompt, def_id?,
  compaction?, timeout_ms?, untrusted?}` with the same meaning as on `spawn`.
  An entry's `untrusted` text goes to that child only. Do not also
  pass a top-level `name`, `prompt` or `def_id`.
- `timeout_ms` — the bound for every entry that has no `timeout_ms` of its
  own. A child that runs out is cancelled and reported in its row; the others
  keep running. Absent or 0 = no bound.
- `mode` — `wait` (default): the call returns when every child has finished.
  `poll`: the call returns at once with each child's `child_run_id`, the
  children work in the background, and you read them with `poll` (by
  `batch_id`, by id, or all unread). Refused on your last iteration.
- `notify` — poll mode only. `true` (default): a short note on your next turn
  names the children that finished since your last one. `false`: no notes.
- `on_parent_end` — poll mode only. `wait` (default): ending your turn while
  any of them still runs makes your run wait for all of them, then gives you
  one more turn. `cancel`: they are cancelled when you end your turn.

The children run concurrently — by default at most 4 at a time; the rest wait
for a free slot. A child's `timeout_ms` starts when it gets its slot, not
while it waits for one, and neither time it is held for review nor time the
runtime is paused counts.

## Returns

`{"results": [{index, agent, ok, output?, error?, state?, run_id?, status?, truncated?, state_omitted?, state_bytes?}]}`,
in the same order as `spawns`. `output` is the child's final text (as `spawn`
returns it) when `ok` is true; `error` says why when it is false. `run_id` is
the child's run, present whenever one was started — failed children included.
`status` is `"timeout"` on a row whose child its `timeout_ms` stopped (`ok` is
false and its run was cancelled), and `"max_iterations"` on a row whose child
stopped at its iteration limit before it finished: `ok` is false, `error`
says so, and its last answer is still in `output` — it may be incomplete.
`status` is absent otherwise. The rows share a quarter of your context window
equally: an `error` or `output` longer than its share is cut, and a `state`
that does not fit is left out whole, replaced by `state_omitted: true` and its
size in `state_bytes`. A cut row carries
`truncated: true` — the child's whole answer, state included, stays in the
transcript of its `run_id`.

In poll mode the call returns `{"batch_id": "fan_...", "children": [{index,
agent, child_run_id, state}]}` at once. `state` is `"running"`, or `"queued"`
for a child waiting for a concurrency slot — it starts when a sibling ends,
after the call has returned. Every entry counts toward your live children
from the start.

## Errors

- `spawns[N]: missing required field: name` / `prompt` — a bad entry refuses
  the whole call before any child starts. Fix that entry.
- `op=parallel_spawn must not carry top-level name/prompt/def_id fields` — put
  them inside the `spawns` entries.
- More than 32 entries is refused; split the batch.
- `spawns[N].timeout_ms=... is above this runtime's ceiling of M ms` — pass at
  most M.
- `this run has N children alive and may have at most M at once; starting K
  more would exceed that` — the whole call is refused before any child
  starts. Every entry counts, queued ones included. Wait for children to
  finish, or send fewer entries.
- A child's own failure is not an error of the call — it is an entry with
  `ok: false`.
- `mode "poll" is refused on your last iteration ...` — use `mode: "wait"`.

## Examples

Research three topics at once, then read each entry's `ok`:

```json
{"op": "parallel_spawn", "spawns": [
  {"name": "researcher", "prompt": "Summarise 2026 pricing for managed Postgres on AWS, GCP and Azure."},
  {"name": "researcher", "prompt": "Summarise 2026 pricing for managed Redis on AWS, GCP and Azure."},
  {"name": "summarizer", "prompt": "List the five most common reasons teams leave managed databases."}]}
```

```json result
{"results": [
  {"index": 0, "agent": "researcher", "ok": true, "output": "[sub-agent agent_id=a_... run_id=r_...]\n...", "run_id": "r_51c0e7a9d2b84f36"},
  {"index": 1, "agent": "researcher", "ok": false, "error": "sub-agent \"researcher\" failed (...): ...", "run_id": "r_8d3f6a1b0c9e2754"},
  {"index": 2, "agent": "summarizer", "ok": true, "output": "[sub-agent agent_id=a_... run_id=r_...]\n...", "run_id": "r_e04b7c2f91a6d538"}]}
```

Start three children in the background (by default 4 run at once) and keep
working:

```json
{"op": "parallel_spawn", "mode": "poll", "spawns": [
  {"name": "researcher", "prompt": "Summarise 2026 pricing for managed Postgres on AWS, GCP and Azure."},
  {"name": "researcher", "prompt": "Summarise 2026 pricing for managed Redis on AWS, GCP and Azure."},
  {"name": "summarizer", "prompt": "List the five most common reasons teams leave managed databases."}]}
```

```json result
{"batch_id": "fan_3c9e1a7b5d2f8064", "children": [
  {"index": 0, "agent": "researcher", "child_run_id": "r_51c0e7a9d2b84f36", "state": "running"},
  {"index": 1, "agent": "researcher", "child_run_id": "r_8d3f6a1b0c9e2754", "state": "running"},
  {"index": 2, "agent": "summarizer", "child_run_id": "r_e04b7c2f91a6d538", "state": "running"}]}
```

Give every child five minutes, and one slow child ten:

```json
{"op": "parallel_spawn", "timeout_ms": 300000, "spawns": [
  {"name": "researcher", "prompt": "Summarise the 2026 changelog of Kubernetes."},
  {"name": "researcher", "prompt": "Summarise every 2026 CVE in OpenSSL.", "timeout_ms": 600000}]}
```

```json result
{"results": [
  {"index": 0, "agent": "researcher", "ok": true, "output": "[sub-agent agent_id=a_... run_id=r_...]\n...", "run_id": "r_0c5e9a7f13b2d846"},
  {"index": 1, "agent": "researcher", "ok": false, "error": "sub-agent \"researcher\" timed out: timeout_ms=600000 elapsed (time held for review or paused not counted); its run r_93d1f0b6a2e4c758 was cancelled", "run_id": "r_93d1f0b6a2e4c758", "status": "timeout"}]}
```
