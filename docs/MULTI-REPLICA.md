# Multi-replica deployment guide

> v0.12.x cluster mode + v1.0 capstone — running 2+ loomcycle replicas behind a load balancer with a shared Postgres database.

## TL;DR

Set `LOOMCYCLE_REPLICA_ID` on each replica (a UUID or short label like `replica-a`), point them all at the same Postgres, and put them behind any HTTP load balancer (no sticky routing needed). Cancel, pause/resume, per-user fairness, run status, hooks, session locks — all work cluster-wide automatically.

```yaml
storage:
  backend: postgres
  pg_dsn: "postgres://loomcycle:secret@db.example.com:5432/loomcycle?sslmode=require"
```

```bash
# Replica A
export LOOMCYCLE_REPLICA_ID=replica-a
export LOOMCYCLE_AUTH_TOKEN=<shared-bearer>
./loomcycle --config loomcycle.yaml

# Replica B
export LOOMCYCLE_REPLICA_ID=replica-b
export LOOMCYCLE_AUTH_TOKEN=<shared-bearer>  # MUST match A
./loomcycle --config loomcycle.yaml
```

LB: round-robin or least-connections, any HTTP load balancer. **No sticky sessions required.**

## What's shared via Postgres

| Concern | Mechanism | Phase introduced |
|---|---|---|
| Run state + transcripts | Existing `runs` / `events` tables | v0.5.x |
| Memory, channels, agent_defs, interruptions | Existing substrate tables | v0.7.x–v0.9.x |
| **Replica heartbeats** | `replicas` table; 30s tick; auto-deleted on shutdown | **v0.12.0** |
| **Cross-replica cancel** | `loomcycle.cancel` LISTEN/NOTIFY + 5s ack timeout | **v0.12.2** |
| **Cluster-wide pause state** | `runtime_state` singleton table + 1s cache + LISTEN/NOTIFY invalidation | **v0.12.3** |
| **Run-state SSE fanout** | `loomcycle.runstate` LISTEN/NOTIFY | **v0.12.3** |
| **Channel notify fanout** | `loomcycle.channel` LISTEN/NOTIFY | **v0.12.3** |
| **Per-user fairness** | `user_quotas` table with atomic UPDATE | **v0.12.1** |
| **Dead-replica reaping** | `coord.ReplicasSweeper` every 60s, 90s stale threshold | **v0.12.4** |
| **Singleton sweepers** | `pg_try_advisory_lock` wrapping heartbeat/memory/channels/interrupts/metrics/dynamic_agents | **v0.12.4** |
| **Session-continuation lock** | `pg_try_advisory_lock(hash(session_id))` on a pinned conn | **v0.12.5** |
| **Tool-use hooks** | `hooks` table + `loomcycle.hook` LISTEN/NOTIFY cache invalidation | **v0.12.5** |

## What stays per-replica

By design, these are not cluster-wide:

- **MCP stdio child processes.** Each replica spawns its own stdio MCP children (resource scaling, not correctness).
- **Anthropic OAuth-dev tokens** (`~/.config/loomcycle/anthropic-oauth.json`). Development path only; production uses API keys.
- **Snapshot `--file` restoration.** The snapshot file lives on one replica's disk. The inline `raw_json` body path is cluster-safe.
- **Global concurrency cap.** `LOOMCYCLE_MAX_CONCURRENT_RUNS=10` on a 2-replica deployment means 20 actual concurrent runs. Per-user fairness IS cluster-wide; global cap is per-replica.
- **`/v1/_concurrency/stats` `active`+`queued` counts.** These reflect the replica that handled the request. `per_user` reads the cluster-wide DB counter.

## Deployment checklist

### Required

- [ ] Postgres 12+ shared across all replicas. SQLite refuses to start when `LOOMCYCLE_REPLICA_ID` is set.
- [ ] All replicas have the **same** `LOOMCYCLE_AUTH_TOKEN`, **same** `loomcycle.yaml`, **same** binary version.
- [ ] Each replica has a **unique** `LOOMCYCLE_REPLICA_ID`. UUID4 or `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`.
- [ ] Postgres pool sized to `MaxConcurrentRuns + headroom` per replica. Session-locked continuations pin one connection per active session.
- [ ] HTTP load balancer (any) in front of `/v1/*` and `/ui/*`. No sticky sessions.

### Recommended

- [ ] `LOOMCYCLE_HEARTBEAT_SWEEPER_ENABLED=1` (default) — without it the replicas TTL sweeper still works but stale runs from crashed replicas live longer.
- [ ] OTEL exporter wired (`LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT`) so the cross-replica trace tree is visible.
- [ ] `LOOMCYCLE_METRICS_ENABLED=1` for `/v1/_metrics/*` observability.

### Cluster verification

```bash
# Each replica reports cluster membership on its own /healthz:
curl -H "Authorization: Bearer $TOKEN" https://lb.example.com/healthz | jq
# Should show:
#   {
#     "ok": true,
#     "version": "...",
#     "replica_id": "replica-a",
#     "replicas": [
#       {"id": "replica-a", "hostname": "...", ...},
#       {"id": "replica-b", "hostname": "...", ...}
#     ]
#   }
```

If `replicas[]` shows only one entry, the other replica isn't healthy — check its logs for boot errors.

## Operator runbook

### Rolling upgrade

The pause-resume mechanism enables safe rolling upgrade:

```bash
# 1. Pause the cluster (any replica accepts this; both replicas converge to paused within 1s).
curl -X POST -H "Authorization: Bearer $TOKEN" https://lb.example.com/v1/_pause

# 2. Wait for in-flight tools to drain (pause endpoint returns when done).

# 3. Snapshot (optional but recommended for major version bumps).
curl -X POST -H "Authorization: Bearer $TOKEN" https://lb.example.com/v1/_snapshot \
    -d '{"label":"pre-upgrade-v1.0.1"}'

# 4. Upgrade replicas one at a time. The remaining replicas continue serving the paused state via /healthz.

# 5. After all replicas are on the new binary, resume.
curl -X POST -H "Authorization: Bearer $TOKEN" https://lb.example.com/v1/_resume
```

### Crashed-replica recovery

If a replica crashes (process kill, host outage), the survivors auto-recover:

- Within 90 seconds the replicas TTL sweeper marks the dead replica's `replicas` row stale.
- All runs owned by the dead replica are marked `status=failed` with `error="owner replica died"`.
- Per-user quota slots leaked by the dead replica are decremented (`GREATEST(0, …)` clamps prevent underflow).
- Cancel requests for the dead replica's runs auto-return `cancelled:true, reason:"owner_dead_marked_failed"`.

You do NOT need to manually clean up the DB. Just restart the replica when ready.

### Adding a third (or Nth) replica

Identical to bootstrapping replica B: pick a unique `LOOMCYCLE_REPLICA_ID`, start the binary. It joins the cluster within one heartbeat interval (30s) and is immediately reachable for cancel/pause/SSE/hooks.

### Reducing to single-replica

Shut down the extra replicas. Their `replicas` rows TTL-sweep within 90s. The surviving replica continues serving traffic; cluster-mode code paths remain active until the operator removes `LOOMCYCLE_REPLICA_ID` from its env and restarts.

### Migrating from single-replica → cluster

1. Set `LOOMCYCLE_REPLICA_ID` on the existing instance, restart. It becomes "the only replica" in cluster mode.
2. Verify `/healthz` shows the cluster view.
3. Start replica 2. It joins automatically.

There is no data migration step — the existing Postgres schema is compatible with cluster mode out of the box (each v0.12.x migration is purely additive).

## Operational concerns

### Postgres LISTEN/NOTIFY load

Every replica holds 4–5 long-lived LISTEN connections (one per backplane topic). Postgres handles this easily; typical clusters use < 10K LISTEN/NOTIFY messages per second. If the cluster exceeds this scale, the `coord.Backplane` interface allows a Redis pub/sub implementation to slot in (post-v1.0).

### Connection pool sizing

Session-locked continuations pin one `pgxpool.Conn` per active session. With `MaxConcurrentRuns=32` and a default `pgxpool` size of 32, a fully-saturated replica with 32 active continuations holds all 32 connections. Other operations would queue on `pool.Acquire`. **Set `LOOMCYCLE_PG_MAX_OPEN_CONNS` to `MaxConcurrentRuns × 1.5` per replica to leave headroom for sweepers, heartbeats, and admin queries.**

### Clock skew

The replicas TTL sweeper uses `now() - 90s` computed on the **Postgres side** for the staleness cutoff. Replica's local clock skew vs the DB does NOT cause false stale-marks. Heartbeats also write `now()` from the DB. No NTP requirement beyond what Postgres itself wants.

### Cluster split-brain

There is no split-brain scenario: Postgres is the single source of truth. If the network partition isolates a replica from the DB, its heartbeats stop landing → other replicas reap it within 90s. The isolated replica's local in-process state stays alive but its DB writes fail, so any operator-issued cancel/pause/run-creation against it errors out cleanly.

## Limitations + sharp edges

1. **Postgres is required for cluster mode.** SQLite refuses to start at boot when `LOOMCYCLE_REPLICA_ID` is set.

2. **MCP stdio children are per-replica.** Operators paying attention to memory should size accordingly: `N replicas × M MCP servers` worth of child processes.

3. **OAuth-dev tokens are per-host.** The `anthropic-oauth-dev` provider is single-machine only by design. Production deployments use API keys.

4. **Snapshot `--file` restoration is replica-local.** If you `loomcycle snapshot restore --file /path/to/snapshot.json` against a load-balanced endpoint, the request may land on a replica that doesn't have the file. Use the inline `raw_json` body for cluster-safe restore, or run the CLI against a specific replica's host.

5. **Global concurrency cap is per-replica.** `LOOMCYCLE_MAX_CONCURRENT_RUNS=10` × 2 replicas = 20 cluster-wide. Per-user fairness IS cluster-wide.

6. **Cancel ack timeout is 5 seconds.** A cancel request that doesn't get an ack within 5s returns `{cancelled: false, reason: "owner_replica_unreachable"}` and suggests checking `_health`. Tunable via `LOOMCYCLE_CANCEL_ACK_TIMEOUT_MS`.

7. **Pause cache TTL is 1 second.** Worst case 1s lag between a pause request and a remote replica refusing new runs. Tunable via `LOOMCYCLE_PAUSE_CACHE_TTL_MS`.

8. **Hook registrations are cluster-wide but require Postgres.** Single-replica deployments keep the v0.11.x in-process hook registry (no DB, no backplane traffic).

## Roadmap (post-v1.0)

- **Redis backplane.** When LISTEN/NOTIFY throughput becomes the bottleneck. v1.1+.
- **Automatic rolling upgrade.** Operator pattern today is "pause cluster → upgrade all → resume." A drain-one-at-a-time automation would be additive.
- **Multi-region.** Single Postgres + N replicas in one region today. Multi-region with read replicas of Postgres is a v2.x scope question.
- **`replica_id` on `/v1/agents/{id}` response.** Currently the wire shape doesn't expose which replica owns each run; could help operator debugging.
