---
name: Agent/parallel_spawn
description: "Agent op=parallel_spawn — run several sub-agents concurrently and get every result back in one envelope, one entry per child."
---
`parallel_spawn` fans a batch of independent tasks out to sub-agents that run
at the same time, and returns when **all** of them have finished. One child
failing does not fail the call: **check `ok` on each entry** and retry or skip
only the ones that failed.

## Arguments

- `op` (required) — `parallel_spawn`.
- `spawns` (required) — 1 to 32 entries, each `{name, prompt, def_id?,
  compaction?}` with the same meaning as on `spawn`. Do not also pass a
  top-level `name`, `prompt` or `def_id`.

The children run concurrently — by default at most 4 at a time; the rest wait
for a free slot.

## Returns

`{"results": [{index, agent, ok, output?, error?, state?, run_id?}]}`, in the
same order as `spawns`. `output` is the child's final text (as `spawn` returns
it) when `ok` is true; `error` says why when it is false. `run_id` is the
child's run, present whenever one was started — failed children included.

## Errors

- `spawns[N]: missing required field: name` / `prompt` — a bad entry refuses
  the whole call before any child starts. Fix that entry.
- `op=parallel_spawn must not carry top-level name/prompt/def_id fields` — put
  them inside the `spawns` entries.
- More than 32 entries is refused; split the batch.
- A child's own failure is not an error of the call — it is an entry with
  `ok: false`.

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
