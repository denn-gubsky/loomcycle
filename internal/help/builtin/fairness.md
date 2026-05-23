---
name: fairness
description: Per-tenant fairness on the run-admitting semaphore — solves the multi-user starvation case within the global MaxConcurrentRuns cap.
---
Loomcycle v0.10.1 adds an opt-in per-user cap to the run-admitting
semaphore. It closes the multi-tenant starvation case operators hit
when one user submits a burst large enough to fill the global queue:
without fairness, every other user's run waits behind the burst even
when the noisy user is plainly hogging the substrate.

The cap is **off by default** — `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER`
unset (or yaml `concurrency.max_concurrent_runs_per_user: 0`) means
the global cap is the only gate, exactly as in v0.10.0 and earlier.
Operators opt in by setting a positive value.

## What the cap measures

The cap applies to **active + queued** runs per non-empty `user_id`.
With the cap at 4:

- User A starts 4 runs → all 4 take active slots. ✓
- User A submits a 5th run → 429 with `code: "per_user_quota_exhausted"`.
- User A submits another run before any complete → still 429.
- User A's run 1 finishes → user A's count drops to 3 → a fresh
  submission succeeds (takes the now-free slot or queues).

Including queued in the count is the load-bearing semantic. If only
active runs counted, a noisy user could fill the queue with their own
runs while at active-cap and starve everyone else for the queue's
lifetime. With active+queued counted, the queue stays available for
other users.

## When the cap kicks in

The check happens before the global cap check. Order:

1. **Per-user cap**: if the user is at cap, return 429 immediately. No
   queue, no wait.
2. **Global active**: if there's an open slot, take it.
3. **Global queue**: if the global queue has room, enqueue. The
   per-user counter increments when enqueued so subsequent attempts
   by the same user see the right count.
4. **Backpressure**: if the global queue is also full, return 429 with
   `code: "backpressure"`.

Both 429 responses share status but distinguish via the JSON body's
`code` field. Adapter consumers branch retry strategies on the code:

| Code | When | Recommended retry |
|---|---|---|
| `per_user_quota_exhausted` | This specific user is at cap | Wait `Retry-After` seconds (server hint: 5) then retry. The wait is deterministic — your other in-flight runs will finish on a schedule. |
| `backpressure` | The whole substrate is overloaded | Exponential backoff with jitter. The wait depends on system-wide load, not just your runs. |

## Anonymous calls bypass the check

Requests without `user_id` (system-initiated, background ops, yaml
callers that omit it) bypass the per-user check by design. The
counter is keyed on a non-empty user_id; empty user_id = no per-user
accounting. Operators wanting strict isolation enforce user_id at the
caller layer.

## Sub-agents don't double-count

Sub-runs (spawned via the Agent tool) share the parent's semaphore
slot AND the parent's user_id count. Per-user accounting only happens
at the three top-level run-creation sites (`/v1/runs`,
`/v1/sessions/{id}/messages`, `RunOnce`). A parent run by `user_a`
that spawns 5 cv-adapter children consumes 1 slot + 1 per-user count,
not 6.

## Enable the cap

**Via env var** (containerized deploys, override yaml without
editing the mounted file):

```sh
LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER=4 ./bin/loomcycle --config loomcycle.yaml
```

**Via yaml**:

```yaml
concurrency:
  max_concurrent_runs: 8                  # global cap (existing)
  max_queue_depth: 16                     # queue depth (existing)
  queue_timeout_ms: 30000                 # max wait per acquire (existing)
  max_concurrent_runs_per_user: 4         # v0.10.1: per-tenant cap (NEW)
```

Restart loomcycle. On boot, a log line confirms the cap is active:

```
concurrency: per-user cap enabled — max_concurrent_runs_per_user=4 (global cap=8)
```

When the env var or yaml value is 0 (the default), no log line —
existing deployments see no behavior change on upgrade.

## Validate fairness from observability

Two surfaces let you confirm the cap is engaging as expected:

**1. `GET /v1/_concurrency/stats`** — bearer-authed admin endpoint
returning the live snapshot:

```sh
$ curl -s -H "Authorization: Bearer $T" http://localhost:8787/v1/_concurrency/stats
{
  "active": 5,
  "queued": 0,
  "per_user": {
    "user_a": 4,
    "user_b": 1
  }
}
```

`per_user` is omitempty — the field is absent when no per-user activity
has happened (cap unconfigured, or no users have hit the substrate
yet). At a glance you see whether a specific user is at their cap.

**2. OTEL `loomcycle.queue_wait_ms` span attribute** — when OTEL is
enabled (v0.10.0+), the top-level `loomcycle.run` span carries the
time spent waiting on the semaphore. Operators graphing this
attribute per `loomcycle.user_id` validate that queue waits
distribute fairly across users instead of all landing on whoever's
behind a noisy tenant.

A useful Jaeger query: `loomcycle.user_id=user_a queue_wait_ms>1000` —
runs by user_a that waited more than a second. If this is concentrated
on one tenant while others see 0 wait, fairness is doing its job.

## Choosing a cap value

Pragmatic starting point: **MaxConcurrentRunsPerUser ≈ MaxConcurrentRuns / 2**.

- Global cap = 8, per-user cap = 4: any user can use up to half the
  substrate; a noisy user can't monopolize.
- Global cap = 64, per-user cap = 8: roomy. Many concurrent tenants;
  no single one dominates.

If you have a known tenant-count and steady load:

- Per-user cap ≈ global cap / max_concurrent_users (rounded up).

The cap can be tightened later without code changes — operators
running `LOOMCYCLE_MAX_CONCURRENT_RUNS_PER_USER=4` today and seeing
sustained per-user pressure at 8 can bump to 6 and restart.

## What this does NOT do (deferred)

- **Queue-reorder fairness.** When the global queue is non-empty, FIFO
  order applies regardless of per-user counts. A user submitting first
  gets the slot first. Queue-reorder (round-robin across users in the
  queue) is a smaller follow-up win not worth bundling with the hard
  cap — but operators wanting it can ask.
- **Per-tier fairness.** The `user_tier` field on `RunIdentity` could
  drive a tier-aware quota (free=2, paid=10). Same architecture, more
  knobs. Defer until a consumer asks.
- **Dynamic cap updates without restart.** `MaxConcurrentRunsPerUser`
  is read at boot. A `POST /v1/_concurrency/limits` admin endpoint
  would close this gap; defer until needed.

## Related topics

- `observability` — OTEL spans + the `loomcycle.queue_wait_ms`
  attribute that backs the Jaeger validation flow above.
- `loomcycle` — the substrate stance + general architecture; explains
  why fairness lives at the semaphore boundary rather than higher up
  in the stack.
