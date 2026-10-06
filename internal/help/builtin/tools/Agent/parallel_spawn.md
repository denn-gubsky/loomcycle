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
  compaction?, timeout_ms?}` with the same meaning as on `spawn`. Do not also
  pass a top-level `name`, `prompt` or `def_id`.
- `timeout_ms` — the bound for every entry that has no `timeout_ms` of its
  own. A child that runs out is cancelled and reported in its row; the others
  keep running. Absent or 0 = no bound.

The children run concurrently — by default at most 4 at a time; the rest wait
for a free slot. A child's `timeout_ms` starts when it gets its slot, not
while it waits for one, and time it is held for review does not count.

## Returns

`{"results": [{index, agent, ok, output?, error?, state?, run_id?, status?}]}`,
in the same order as `spawns`. `output` is the child's final text (as `spawn`
returns it) when `ok` is true; `error` says why when it is false. `run_id` is
the child's run, present whenever one was started — failed children included.
`status` is `"timeout"` on a row whose child its `timeout_ms` stopped (`ok` is
false and its run was cancelled); it is absent otherwise.

## Errors

- `spawns[N]: missing required field: name` / `prompt` — a bad entry refuses
  the whole call before any child starts. Fix that entry.
- `op=parallel_spawn must not carry top-level name/prompt/def_id fields` — put
  them inside the `spawns` entries.
- More than 32 entries is refused; split the batch.
- `spawns[N].timeout_ms=... is above this runtime's ceiling of M ms` — pass at
  most M.
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

Give every child five minutes, and one slow child ten:

```json
{"op": "parallel_spawn", "timeout_ms": 300000, "spawns": [
  {"name": "researcher", "prompt": "Summarise the 2026 changelog of Kubernetes."},
  {"name": "researcher", "prompt": "Summarise every 2026 CVE in OpenSSL.", "timeout_ms": 600000}]}
```

```json result
{"results": [
  {"index": 0, "agent": "researcher", "ok": true, "output": "[sub-agent agent_id=a_... run_id=r_...]\n...", "run_id": "r_0c5e9a7f13b2d846"},
  {"index": 1, "agent": "researcher", "ok": false, "error": "sub-agent \"researcher\" timed out: timeout_ms=600000 elapsed (time held for review not counted); its run r_93d1f0b6a2e4c758 was cancelled", "run_id": "r_93d1f0b6a2e4c758", "status": "timeout"}]}
```
