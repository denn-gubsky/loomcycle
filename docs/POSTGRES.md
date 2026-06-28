# Postgres backend (v0.5.0)

This guide is for operators running loomcycle in production. SQLite stays the default — it covers compact installs, dev environments, and the [README](../README.md) quick-start with zero external dependencies. **Switch to Postgres when you need horizontal scaling** (multiple loomcycle replicas behind a load balancer, hundreds of concurrent agents, shared-DB across services). The agent-facing wire protocol and the `Store` interface are identical between backends; the operator-visible difference is the deployment topology.

For the runtime architecture, see [ARCHITECTURE.md](ARCHITECTURE.md). For the public roadmap (v0.5.0 milestones), see [PLAN.md](PLAN.md).

## When to choose Postgres

| Situation | Recommendation |
|---|---|
| Single laptop / dev environment | **SQLite.** Default. Zero deps. |
| Single production server, ≤ 100 concurrent agents | **SQLite or Postgres.** SQLite is simpler; Postgres is forward-leaning. |
| Single production server, hundreds of concurrent agents | **Postgres.** SQLite's single-writer becomes the bottleneck under burst load. |
| Multiple replicas / horizontal scale | **Postgres.** SQLite has no story here; Postgres + advisory locks (v1.0) does. |
| You already run Postgres for the rest of your stack | **Postgres.** Avoid the per-replica SQLite file. |

## Configuration

Two paths into the Postgres backend — operator picks whichever fits the deploy:

**Yaml (good for dev, version-controlled):**

```yaml
storage:
  backend: postgres
  pg_dsn: postgres://loomcycle:secret@db.internal:5432/loomcycle?sslmode=require
  pg_max_open_conns: 32
  pg_min_idle_conns: 8
  pg_automigrate: false
```

**Env (good for prod — keeps secrets out of the repo):**

```sh
export LOOMCYCLE_STORAGE_BACKEND=postgres
export LOOMCYCLE_PG_DSN="postgres://loomcycle:$(cat /run/secrets/pg_password)@db.internal:5432/loomcycle?sslmode=require"
export LOOMCYCLE_PG_AUTOMIGRATE=0
```

Env wins over yaml when both are set. Defaults (omit to use these):

| Setting | Default |
|---|---|
| `pg_max_open_conns` | 32 |
| `pg_min_idle_conns` | 4 |
| `pg_automigrate` | false |

**Postgres ≥ 14 required** — that's the floor, not a pin. loomcycle uses no
version-locked SQL features, so any newer major (15, 16, 17, 18, …) works
unchanged; pick whatever your platform ships. The `postgres:16` images in the
example composes are a tested default, not a requirement. The one version-gated
piece is **pgvector** (only needed for Vector Memory / `recall`): it supports
PostgreSQL 13–18, so install the extension package matching your server's major
(e.g. `postgresql-18-pgvector`, or a `pgvector/pgvector:pg18` image). Without
pgvector, the embeddings table is skipped gracefully and everything else runs.

## Schema migrations

Migrations are embedded in the binary via `golang-migrate/migrate v4`. Two policies:

### `LOOMCYCLE_PG_AUTOMIGRATE=1` (dev-friendly)

Loomcycle runs `migrate up` on startup before serving traffic. A fresh DB gets the schema; subsequent restarts are a no-op (golang-migrate's `schema_migrations` table tracks applied versions). Easy to set up, dangerous in production — a buggy migration in a new release crashes the rollout.

### `LOOMCYCLE_PG_AUTOMIGRATE=0` (production default)

Loomcycle refuses to start unless the embedded migration set is at-or-behind the database. The operator runs `loomcycle migrate up` explicitly. Production rollouts gain:

- **Migration is decoupled from binary deploy.** Apply migrations in their own change-management window before flipping the new binary live.
- **Rollback safety.** A new binary that adds migrations can be safely rolled back to the old binary as long as the schema isn't ahead of the old code's expectations.
- **Forward compat.** The new binary's `VerifySchemaCurrent` accepts a database whose version is ≥ the embedded set, so a parallel-deploy scenario where the new schema lands first doesn't break the older replicas.

### Subcommands

```sh
loomcycle migrate up      [--config <yaml>] [--dsn <dsn>]   # apply pending
loomcycle migrate down    [--config <yaml>] [--dsn <dsn>]   # roll back (requires --yes)
loomcycle migrate status  [--config <yaml>] [--dsn <dsn>]   # version + dirty flag
```

DSN precedence: `--dsn` flag > `LOOMCYCLE_PG_DSN` env > yaml `storage.pg_dsn`. Exit codes: `0` success, `1` operational failure (migration error, PG unreachable), `2` user error (bad flags, missing DSN).

`migrate down` requires `--yes` because rolling every applied migration back drops every table. Refusing the unconfirmed call is a guardrail against `loomcycle migrate down` getting tab-completed by mistake.

## Migrating existing SQLite data

Operators with existing SQLite transcripts can copy them into Postgres without losing history. Standard runbook:

```sh
# 1. Stop loomcycle. (Live migration is out of scope for v0.5.0.)
systemctl stop loomcycle

# 2. Copy the SQLite file. Always run the migration against a copy,
#    not the live DB — SQLite's WAL + busy-timeout semantics race
#    badly with a long-running read.
cp /var/lib/loomcycle/loomcycle.db /var/lib/loomcycle/loomcycle.db.copy

# 3. Apply the schema to the destination Postgres. Use the operator's
#    real DSN; the example below is for the local fixture.
loomcycle migrate up \
  --dsn "postgres://loomcycle:secret@db:5432/loomcycle?sslmode=require"

# 4. Run the data copy. Each row uses ON CONFLICT (id) DO NOTHING so
#    a partial-failure re-run resumes cleanly. Verify phase
#    cross-checks row counts + sha256-digests 10 random session
#    transcripts byte-equal between adapters.
loomcycle migrate sqlite-to-postgres \
  --src /var/lib/loomcycle/loomcycle.db.copy \
  --dst "postgres://loomcycle:secret@db:5432/loomcycle?sslmode=require"

# 5. Flip the operator yaml's storage.backend to postgres (or set
#    LOOMCYCLE_STORAGE_BACKEND=postgres). Restart loomcycle.
systemctl start loomcycle
```

Out of scope for v0.5.0:

- **Live cutover.** Loomcycle stays stopped during the copy.
- **Postgres → SQLite reverse direction.** Once you're on Postgres, you've crossed the SQLite-ceiling threshold; the reverse direction is rarely useful.
- **Schema drift handling.** The migration tool assumes the destination schema matches the source's. If you upgrade loomcycle versions across the migration, run `migrate up` on the new schema BEFORE the data copy.

## Concurrency benchmark — the v0.5.0 reference numbers

Synthetic load measured on Apple M1 (8-core), `postgres:16-alpine` in Docker, no network distance to the DB. Operators should re-run on their own deployment for change-management; the numbers below are reference points, not a target.

```sh
# SQLite baseline
go test -run='^$' -bench=BenchmarkConcurrentRuns -benchtime=1x ./internal/store/sqlite/...

# Postgres (live fixture — make pg-up first)
LOOMCYCLE_TEST_PG_DSN="postgres://loomcycle:loomcycle@127.0.0.1:5432/loomcycle_test?sslmode=disable" \
  go test -run='^$' -bench=BenchmarkConcurrentRuns -benchtime=1x ./internal/store/postgres/...
```

Workload: 100 concurrent agents, each running 1 session + 1 run + 50 events + 1 finish. Total: 100 runs, 5,000 events, 1.4 MB of payloads.

| Backend | Wall | Runs/s | Events/s | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| SQLite | 443 ms | 226 | 11,283 | 6 ms | 19 ms | 31 ms |
| Postgres | 911 ms | 110 | 5,489 | 15 ms | 33 ms | 60 ms |

**Reading the numbers:** SQLite is faster at this concurrency tier because it's in-process — no network round-trip per query, no TCP handshake, no pgxpool acquisition latency. The Postgres adapter pays the network cost in exchange for everything you get above SQLite's ceiling: multi-replica scale, shared-DB topology, mature backup tooling, advisory locks for v1.0 cross-replica work.

**Where Postgres overtakes SQLite:** sustained burst load above SQLite's single-writer threshold. Concrete signal in production: SQLITE_BUSY errors in the loomcycle log, or `last_heartbeat_at` falling behind under burst because the writer is queued on the SQLite mutex.

**Acceptance threshold for v0.5.0:** the Postgres path's p99 must stay under 1 second at this concurrency tier. The 60 ms measured leaves a wide margin for slower production environments (network distance, contended Postgres host, SSL overhead). Operators seeing p99 > 500 ms on their own deploy should investigate before scaling further: connection pool sizing, Postgres host CPU, network distance.

## Heartbeat sweeper + session-lock GC interaction

Two background goroutines (covered in [PLAN.md v0.5.0 section](PLAN.md)) run alongside the HTTP server regardless of backend:

- **Heartbeat sweeper** — periodic `UPDATE runs SET status='failed' WHERE status='running' AND last_heartbeat_at < cutoff`. Postgres handles concurrent sweepers correctly via the WHERE clause; future multi-replica deployments can have one designated sweeper (`LOOMCYCLE_HEARTBEAT_SWEEPER=0` everywhere else) or let every replica run the sweep harmlessly.
- **Session-lock map GC** — pure in-memory; one map per loomcycle process. Per-replica state, not shared via Postgres.

## Operational notes

**Connection pool sizing.** Default `MaxConns=32` is right for a single-replica deployment hitting hundreds of concurrent agents. Each agent run holds at most one connection during a query (CreateRun, AppendEvent, etc.); parallel `AppendEvent` from the loop can briefly need more. If you see `pgxpool: ErrAcquireTimedOut` in the loomcycle log under burst, raise `LOOMCYCLE_PG_MAX_OPEN_CONNS`.

**SSL.** Operators connecting across a network should use `sslmode=require` or stronger. The example yaml uses `sslmode=require`; the `make pg-up` local fixture uses `sslmode=disable` because the container is loopback-bound.

**TLS termination.** Loomcycle doesn't expose Postgres traffic; the connection is its own concern. If your Postgres is behind a TLS-terminating proxy (Cloud SQL Auth Proxy, AWS RDS Proxy), point the DSN at the proxy.

**Logical backups.** Standard `pg_dump` works. Loomcycle has no streaming-replication awareness in v0.5.0; physical replication (e.g. AWS Aurora multi-AZ) is the operator's choice and orthogonal to the runtime.

**Schema drift detection.** The CLI's `loomcycle migrate status` is the canonical way to read the current schema version. Pipelines can grep for `dirty: false` to assert health before promoting traffic.
