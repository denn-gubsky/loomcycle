---
name: fairness
description: How runs are admitted when the runtime is busy — the run queue, the per-user cap, and admission "wait" vs "reject".
---
A run that cannot start at once is **queued**, not refused. It gets a run
row in status `queued` and a `queued` event right away, so the caller has
its ids and can read, cancel or re-attach to it, and it starts when a run
slot frees. A queued run never times out: it waits until it starts, it is
cancelled, or its caller (for a run bound to its caller's connection)
goes away.

Two limits decide whether a run waits:

- **The global cap** (`concurrency.max_concurrent_runs`): every run slot
  is taken. The run waits with reason `global_cap`.
- **The per-user cap** (`concurrency.max_concurrent_runs_per_user`, off
  by default): its (tenant, user) already has that many runs running or
  waiting for a slot. The run waits with reason `user_cap` until one of
  that user's runs ends.

## The queue is fair across users

Waiting runs are served **round-robin across (tenant, user)**, FIFO
within one user. When a slot frees, the next user in turn gets it, not
the oldest request overall: a user who queued a burst of 20 runs first
does not hold back a user who queued one run after them. A run that was
already admitted once (a run resumed after a pause or a restart) is
served first.

## What bounds the queue

| Limit | Default | Past it |
|---|---|---|
| `concurrency.max_queue_depth` | 32 | a new run is refused: 429 `backpressure` |
| `concurrency.max_queued_runs_per_user` | 16 | a run over its user's cap is refused: 429 `per_user_quota_exhausted` |

`max_queued_runs_per_user: 0` refuses a run over the per-user cap at
once, with no waiting. In cluster mode it is counted per replica.

With no per-user cap, `max_queued_runs_per_user` also bounds how many
places one (tenant, user) may hold in the queue for a run slot; runs that
name no user share their tenant's places. Past it the run is refused with
429 `backpressure`, so one caller cannot fill the queue and turn every
other tenant away.

## admission: "wait" or "reject"

Every way of starting a run (POST /v1/runs, the gRPC Run RPC, the MCP
`spawn_run` / `spawn_runs` tools, the batch endpoint, the TS and Python
clients) takes `admission`:

- `"wait"` (the default) queues the run as above.
- `"reject"` refuses it at once instead, with the codes below — for a
  caller that would rather retry elsewhere than wait.

| Code | When | Recommended retry |
|---|---|---|
| `per_user_quota_exhausted` | This user is at its cap | Wait `Retry-After` seconds (server hint: 5) then retry; your own runs free the slot. |
| `backpressure` | No run slot is free, or the queue is full | Exponential backoff with jitter; the wait depends on system-wide load. |

A session continuation on POST /v1/sessions/{id}/messages queues the same
way: its stream opens with `session`, `agent` and `queued` while it waits,
and it keeps the session until it has run, so another message to the same
session meanwhile is answered 409 `session_busy`. It always waits;
`admission` applies to a fresh run only. A request to the LLM gateway does
not queue yet: it waits up to `concurrency.queue_timeout_ms` and then
answers 429.

## Seeing a queued run

- The run's stream carries `queued` (with `admission.reason` and
  `admission.position`, 1 = next) and, when it starts, `admitted` (with
  `admission.wait_ms`). Both are in the run's transcript.
- Once it runs, each of its model calls that has to wait for its
  provider (the provider's call lane is full, or it has its cap of
  models in flight) reports `call_queued` (`admission.reason` `lane` or
  `models_in_flight`, `admission.position`, `admission.provider`,
  `admission.kind`) and then `call_admitted` (`admission.wait_ms`), on
  the stream and in the transcript. That includes a decision or an
  embedding one of its tools makes (`kind` `decision` / `embedder`);
  background work started from the run does not report on it.
- A single-run read (HTTP GET /v1/runs/{run_id} or /v1/agents/{agent_id}, gRPC GetRun, MCP `get_run`)
  reports `status: "queued"`, `queue_position` and `queue_reason` while
  it waits. For `user_cap` the position counts only that user's waiting
  runs.
- A run still queued when loomcycle stops is queued again at the next
  boot, under the same run id, in the order it first queued, and runs to
  its end. Its caller's connection ended with the stop: read it, or
  re-attach to its stream, by run id. A retry with the same
  `idempotency_key` gets that run back; a retry without a key starts a
  second one.
- Its request is stored on its row while it waits, its `user_bearer` and
  `user_credentials` sealed with `LOOMCYCLE_SECRET_KEY`. A queued run
  that cannot come back is failed at the boot with stop reason
  `restarted`, a reason saying why, and its idempotency key released: a
  run with per-run secrets when no `LOOMCYCLE_SECRET_KEY` is set (they
  are never stored in plaintext), a no-trace run (its prompt is stored
  nowhere), or one whose sealed secrets the key at boot cannot open.
- A paused runtime starts no queued run: one whose slot frees during
  the pause keeps it, stays `queued`, and starts on the resume. A
  snapshot carries the queued runs, without their per-run secrets; a
  restore queues them again, and they run without those secrets.

## Anonymous calls bypass the per-user cap

Requests without `user_id` (system-initiated, background ops, yaml
callers that omit it) are not counted per user. They still take a run
slot and wait in the queue like any run.

## Sub-agents don't double-count

Sub-runs (spawned via the Agent tool) share the parent's run slot and
its per-user count. A parent run by `user_a` that spawns 5 children
takes 1 slot and 1 per-user count, not 6.

## Configure

```yaml
concurrency:
  max_concurrent_runs: 8              # global cap
  max_queue_depth: 32                 # waiting runs, all reasons together
  max_concurrent_runs_per_user: 4     # per-user cap (0 = off)
  max_queued_runs_per_user: 16        # waiting runs per user over its cap
```

Env overrides: `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER`,
`LOOMCYCLE_MAX_QUEUED_RUNS_PER_USER`. The caps also change on a config
reload.

## Validate fairness from observability

**`GET /v1/_concurrency/stats`** (admin) returns the live snapshot:
`active`, `queued` (every waiting run, both reasons) and `per_user`
(runs running or waiting for a slot, per user).

**OTEL `loomcycle.queue_wait_ms`** on the top-level `loomcycle.run`
span is the time the run waited for its slot. Graphed per
`loomcycle.user_id`, it shows whether waits spread across users or land
on whoever is behind a noisy one.

## Choosing a cap value

Pragmatic starting point: **max_concurrent_runs_per_user ≈
max_concurrent_runs / 2** — any user can use up to half the runtime, and
no single one can take it all. With a known number of concurrent users,
per-user cap ≈ global cap / users, rounded up.

## Related topics

- `observability` — OTEL spans and the `loomcycle.queue_wait_ms`
  attribute.
- `loomcycle` — the substrate stance and general architecture.
