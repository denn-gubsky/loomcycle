# SQL Memory (RFC AA)

SQL Memory is a facet of the built-in `Memory` tool that lets **authorized**
agents run arbitrary SQL against a **per-scope database the runtime hosts**,
isolated from the main loomcycle store. It exists for sandboxed / short-lived
agents that need related tables, joins, and aggregates — structured storage the
K/V and vector Memory can't give — *without* the `Bash + sqlite3` cost and
process-isolation hole (`bash.go` is explicitly not a sandbox).

**Why a separate store, not "just run `psql`/`sqlite3` in Bash":** the agent
never gets a shell, never needs an external DB or credential bootstrap, and
every statement is bounded (timeout + row cap + byte quota) and **fully
audited**. The threat model is *not* SQL injection — the agent is authorized to
run SQL — it is **escaping the scope** (reaching another scope's data, host
files, code execution, or the operational DB) and **resource exhaustion**.

## Surface

Two `Memory` ops (the `Memory` tool must be in the agent's `allowed_tools`):

- `op: "sql_exec"` — one DDL/DML statement (`CREATE TABLE`/`INSERT`/`UPDATE`/
  `DELETE`/…), `args` for `?`/`$1` bind params; returns `{rows_affected,
  last_insert_id?}` (`last_insert_id` is sqlite-only — use `RETURNING` on
  postgres).
- `op: "sql_query"` — one read-only `SELECT` / `WITH … SELECT`; returns
  `{columns, rows, truncated}` (capped at `sqlmem_max_rows`).

`scope` selects the database: `agent` (this agent, durable), `user` (this
end-user, durable), or `run` (ephemeral scratch, dropped at run completion).
Durable scopes are keyed by the authoritative tenant. One statement per call;
`ATTACH`/`PRAGMA`/`load_extension`/`COPY`/`SET`/multiple statements are refused.

For **atomic multi-step writes**, three more ops manage a transaction:

- `op: "sql_begin"` — open a transaction for the scope. From here, `sql_exec` /
  `sql_query` on that scope (in this run) run **on** the transaction.
- `op: "sql_commit"` / `op: "sql_rollback"` — finish it.

See [Explicit transactions](#explicit-transactions) below.

## Capability gate — default-deny `sql_scopes`

SQL Memory is **off** unless the operator enables the subsystem *and* the agent
declares which scopes it may touch (the RFC W capability-gate pattern — having
`Memory` in `allowed_tools` is **not** sufficient):

```yaml
# loomcycle.yaml
storage:
  sqlmem_enabled: true            # or LOOMCYCLE_SQLMEM_ENABLED=1

agents:
  research-bot:
    allowed_tools: [Memory, Read, Grep]
    sql_scopes: [agent, run]      # closed enum {agent,user,run}; empty => every SQL op refuses
    sql_quota_bytes: 52428800     # optional per-agent override of the global quota
```

## Storage tiers — SQL Memory follows the main backend

| Main `storage.backend` | SQL Memory tier | Isolation |
|---|---|---|
| `sqlite` (default) | one **file per scope** under `<DataDir>/sqlmem/` | a separate `.db` per scope (single-replica) |
| `postgres` | one **schema per scope** in a **separate aux database** | a per-scope least-privilege role (multi-replica) |

### Configuration (env / yaml)

| Env | yaml (`storage:`) | Meaning |
|---|---|---|
| `LOOMCYCLE_SQLMEM_ENABLED=1` | `sqlmem_enabled` | turn the subsystem on (off by default) |
| `LOOMCYCLE_SQLMEM_ROOT` | `sqlmem_root` | sqlite tier: parent dir for `.db` files (default `<DataDir>/sqlmem`) |
| `LOOMCYCLE_SQLMEM_PG_DSN` | `sqlmem_pg_dsn` | **postgres tier: the SEPARATE aux-DB DSN (required on the postgres backend)** |
| `LOOMCYCLE_SQLMEM_QUOTA_BYTES` | `sqlmem_quota_bytes` | per-scope size cap (0 = none); per-agent `sql_quota_bytes` overrides |
| `LOOMCYCLE_SQLMEM_STATEMENT_TIMEOUT_MS` | `sqlmem_statement_timeout_ms` | per-statement timeout (default 30000) |
| `LOOMCYCLE_SQLMEM_MAX_ROWS` | `sqlmem_max_rows` | `sql_query` row cap (default 10000) |
| `LOOMCYCLE_SQLMEM_AUDIT_MODE` | `sqlmem_audit_mode` | `full` (redacted statement text, default) or `metadata` (counts only) |
| `LOOMCYCLE_SQLMEM_TXN_TIMEOUT_MS` | `sqlmem_txn_timeout_ms` | explicit-transaction TTL — the reaper rolls back a txn open longer than this (default 30000; 0 disables) |
| `LOOMCYCLE_SQLMEM_MAX_OPEN_TXNS` | `sqlmem_max_open_txns` | cap on concurrent open transactions process-wide (default 64; each pins a connection) |
| `LOOMCYCLE_SQLMEM_MAX_TXN_DEPTH` | `sqlmem_max_txn_depth` | cap on SAVEPOINT nesting depth within one transaction (default 16; a nested `sql_begin` past it errors) |
| `LOOMCYCLE_SQLMEM_SCOPE_TTL_MS` | `sqlmem_scope_ttl_ms` | durable-scope GC: drop an `agent`/`user` scope idle longer than this. **0 = OFF** (default — GC discards data) |
| `LOOMCYCLE_SQLMEM_GC_INTERVAL_MS` | `sqlmem_gc_interval_ms` | how often the GC sweeper runs (default 1h; only when the TTL is set) |

`LOOMCYCLE_SQLMEM_PG_DSN` carries the aux credentials — like `LOOMCYCLE_PG_DSN`
it is on the env-expand denylist and is **never** interpolatable into a
`${...}` config/MCP field.

## Durable-scope garbage collection

`agent`/`user` scopes are **durable** — they persist across runs and are never
dropped automatically (only `run` scopes drop at run completion). Over time a
deployment accumulates one scope per distinct agent + per distinct end-user that
ever used SQL Memory. Optional GC reclaims the idle ones.

Set **`sqlmem_scope_ttl_ms`** (off by default) and a background sweeper (every
`sqlmem_gc_interval_ms`, default 1h) drops any durable scope **not used** for
longer than the TTL — the sqlite `.db` file (fenced removal), or the postgres
schema + role. `run` scopes are never touched.

> **GC is lossy — opt in deliberately.** A dropped scope's tables are gone; an
> agent or user that returns *after* the TTL starts empty. Set the TTL generously
> (days/weeks) so "idle past the TTL" reliably means "abandoned." Last-use is
> tracked by the `.db` mtime (sqlite — reads count too) / a `sqlmem_meta`
> bookkeeping table the runtime creates in the aux DB (postgres). GC is global
> across tenants (it only *drops* idle scopes; it never reads their data).

## Explicit transactions

For atomic multi-step writes, an agent opens a transaction with `sql_begin`,
runs any number of `sql_exec` / `sql_query` on the scope, then `sql_commit` or
`sql_rollback`:

```jsonc
{"op":"sql_begin",    "scope":"agent"}
{"op":"sql_exec",     "scope":"agent", "statement":"UPDATE accounts SET bal = bal - 10 WHERE id = 1"}
{"op":"sql_exec",     "scope":"agent", "statement":"UPDATE accounts SET bal = bal + 10 WHERE id = 2"}
{"op":"sql_commit",   "scope":"agent"}    // or sql_rollback to undo both
```

- **Runtime-managed.** The agent never writes a raw `BEGIN`/`COMMIT` (the
  validator refuses those) — the dedicated ops let the runtime own the
  connection-pinning + cleanup. While a transaction is open for a `(run, scope)`,
  that scope's `sql_exec`/`sql_query` run **on** it; with none open they
  auto-commit exactly as before.
- **One open transaction per `(run-tree, scope)`**, at most `sqlmem_max_open_txns`
  process-wide. The key is the run-**tree** root, so within one run tree an open
  transaction on a scope is **shared**: if a parent opens a transaction on a
  scope and a (parallel) sub-agent then runs `sql_exec`/`sql_query` on that *same*
  scope, it runs on the parent's transaction (and is committed/rolled-back with
  it). Agents that must transact independently should use the per-agent `agent`
  scope (distinct per agent name) rather than a shared `user`/`run` scope.
- **Nesting (`SAVEPOINT`).** A second `sql_begin` while a transaction is open
  **nests** — it opens a SAVEPOINT instead of erroring. The matching `sql_commit`
  releases that inner level (merging it up); `sql_rollback` undoes only the inner
  level's writes, with the outer transaction continuing. Each op's result reports
  the current `depth` (1 = the root transaction; 0 = closed). This is the "try a
  sub-step, undo only that on failure" idiom — no agent-managed savepoint names.
  Nesting is LIFO (commit/rollback affect the innermost level) and capped at
  `sqlmem_max_txn_depth` (default 16). A whole-transaction rollback (explicit, a
  run end, or the reaper) discards every savepoint with it.
  ```jsonc
  {"op":"sql_begin",  "scope":"agent"}                                  // depth 1
  {"op":"sql_exec",   "scope":"agent", "statement":"INSERT INTO log VALUES ('a')"}
  {"op":"sql_begin",  "scope":"agent"}                                  // depth 2 (SAVEPOINT)
  {"op":"sql_exec",   "scope":"agent", "statement":"INSERT INTO log VALUES ('b')"}
  {"op":"sql_rollback","scope":"agent"}    // undoes only 'b'; depth → 1, txn still open
  {"op":"sql_commit", "scope":"agent"}     // commits 'a'; depth → 0
  ```
- **Read-write.** A `sql_query` inside a transaction relies on the validator's
  SELECT-only rule for read-safety (not the auto-commit read-only transaction).
- **Always cleaned up.** A transaction is rolled back if the run ends with it
  open, or if it stays open longer than `sqlmem_txn_timeout_ms` (the reaper) — a
  held connection + locks never leak past a stuck agent. Commit before doing
  anything that ends the run.
- **Concurrency / replica notes.** On **sqlite** a scope is a single writer, so
  while one run holds a transaction on a durable scope, *another* run's
  auto-commit op on that same scope blocks until the transaction finishes (its
  ctx deadline still applies). On **postgres** the per-scope pool serves a couple
  of concurrent connections. A transaction is **replica-local**: a run that
  migrates to another replica (steer/resume) orphans its open transaction → it is
  reaped and the continuation auto-commits.

## Vector columns (postgres tier)

An agent can keep **embeddings inside its own SQL tables** — semantic KNN *and*
structured filters/joins in one query. **Postgres only** (pgvector); the sqlite
tier returns a typed "vectors require the postgres tier" refusal.

The agent never handles a raw vector. A bind arg of the form `{"$embed":
"<text>"}` is replaced **server-side** by the embedding of that text (a pgvector
value), so the multi-KB vector never enters the model's context. Reference it
with a `::vector` cast:

```jsonc
// the agent declares the column + (optionally) an index
{"op":"sql_exec","scope":"agent","statement":"CREATE TABLE docs (id serial, body text, lang text, embedding vector(1536))"}
{"op":"sql_exec","scope":"agent","statement":"CREATE INDEX ON docs USING hnsw (embedding vector_cosine_ops)"}
// store a doc + its embedding (no vector in context)
{"op":"sql_exec","scope":"agent","statement":"INSERT INTO docs(body, lang, embedding) VALUES ($1, $2, $3::vector)",
 "args":["the body text","en",{"$embed":"the body text"}]}
// semantic KNN with a structured filter
{"op":"sql_query","scope":"agent","statement":"SELECT id, body FROM docs WHERE lang=$1 ORDER BY embedding <=> $2::vector LIMIT 5",
 "args":["en",{"$embed":"machine learning"}]}
```

Notes:
- The embedding **dimension** is the configured embedder's (`memory.embedder`);
  declare the column to match (`vector(<dim>)`), or use an unsized `vector`.
  Postgres placeholders are `$1, $2, …` (not `?`).
- `$embed` requires a **configured embedder** (`memory.embedder`) AND the
  postgres tier with **pgvector installed** (provisioning below); otherwise the
  op refuses with a message naming the missing piece.
- The agent cannot install the extension (`CREATE EXTENSION` stays denied) — the
  operator installs it once. The `vector` type/operators live in a shared,
  read-only `sqlmem_ext` schema; per-scope isolation is unchanged (your tables
  stay in your own schema).

## Postgres tier — security model

Isolation is **engine-enforced least privilege** (the parsed-statement validator
is defense-in-depth):

- The aux DB is a **different database** from the main loomcycle store, so even a
  hypothetical escape can't reach sessions/runs/tokens/memory.
- Each scope lazily provisions a **schema** + a dedicated **per-scope `LOGIN`
  role** with `USAGE` only on that schema (`PUBLIC` revoked), non-superuser,
  `NOCREATEDB`/`NOCREATEROLE`/`NOINHERIT`, with `search_path` + `statement_timeout`
  baked onto the role.
- The runtime runs the agent's SQL on a **dedicated connection authenticated AS
  that scope role** — so the agent's `session_user` **is** the scope role. This
  is the load-bearing property: a scope role is a member of **nothing**, so every
  PostgreSQL role-switch primitive (`SET ROLE`, `set_config('role',…)`,
  `RESET ROLE`, a function's `SET role` attribute) is checked against the scope
  role and **cannot reach another scope**. (An earlier design that connected as
  one shared admin and `SET LOCAL ROLE`d down to the scope role was *broken* —
  `SET LOCAL ROLE` leaves `session_user` as the admin, which was a member of
  every scope role, so an agent could pivot via a `SET role` function clause.)
  The agent therefore **cannot**:
  - reach another scope's schema — no `USAGE`, even with a fully-qualified
    `other_schema.table` reference (engine: *permission denied for schema*),
    and no role-switch can change that;
  - read host files / run programs — `COPY … TO/FROM PROGRAM`, `COPY … '/file'`,
    `pg_read_file`, `pg_ls_dir`, `lo_import`/`lo_export` are all denied;
  - load code or connect out — `CREATE EXTENSION` / `CREATE LANGUAGE` / `dblink`
    are denied;
  - escalate — the scope role is a member of no privileged role, and the
    operator-provisioned admin role (which CAN provision/drop scopes) **never
    runs agent SQL**, so its authority is unreachable from a statement.
- `sql_query` additionally runs in a **read-only transaction**, so any write the
  validator missed (e.g. `SELECT … INTO`) fails at the engine.
- The schema-size quota uses a `pg_total_relation_size` sweep over the scope
  schema before a write.
- Per-scope role passwords are derived `HMAC(aux-admin-password, role-name)` (so
  every replica computes the same value without coordination); the agent has no
  network path to the aux DB regardless.
- **Known, content-safe metadata leak:** an agent can read the system catalogs
  (`pg_namespace`/`pg_roles`/`pg_largeobject_metadata`) and so *enumerate* other
  scopes' schema/role **names** and large-object oids in the same aux DB. No
  **content** is reachable — cross-schema `USAGE`, large-object bytes, and
  `rolpassword` are all engine-denied — and the names are a one-way SHA-256 of
  `(tenant, scope, scope_id)`, revealing nothing about the victim's identity.

> **Strongest posture:** point `LOOMCYCLE_SQLMEM_PG_DSN` at a **separate
> PostgreSQL instance/cluster** (not just a separate database). The admin role
> below needs `CREATEROLE`, which is cluster-wide; an entirely separate cluster
> removes that surface from the main store's cluster.

## Postgres tier — operator provisioning (one-time)

The runtime provisions per-scope schemas/roles itself, but the operator must
first create the **aux database** and a **non-superuser admin role** the runtime
connects as. Run this as a superuser on the aux cluster:

```sql
-- A non-superuser admin: it can create schemas in the aux DB and create the
-- per-scope roles, but it is NOT a superuser and has no server-file / program /
-- replication powers. NEVER grant it pg_read_server_files,
-- pg_write_server_files, or pg_execute_server_program.
CREATE ROLE loomcycle_sqlmem LOGIN PASSWORD '<strong-secret>' CREATEROLE;

-- A dedicated database, owned by that admin (so it can CREATE SCHEMA in it).
CREATE DATABASE loomcycle_sqlmem OWNER loomcycle_sqlmem;

-- Lock down the public schema of the aux DB (defense-in-depth; scopes never use it).
\connect loomcycle_sqlmem
REVOKE ALL ON SCHEMA public FROM PUBLIC;

-- OPTIONAL — vector columns (Phase 3c). Install pgvector into a dedicated
-- read-only schema the scope roles can USE. Run as a role that may CREATE
-- EXTENSION (e.g. a superuser); the runtime detects this at startup and bakes
-- sqlmem_ext onto each scope role's search_path.
CREATE SCHEMA sqlmem_ext;
CREATE EXTENSION vector SCHEMA sqlmem_ext;
GRANT USAGE ON SCHEMA sqlmem_ext TO PUBLIC;  -- only the type/operators; no data
```

Then point the runtime at it (keep the DSN in `.env.local`, not yaml):

```bash
# .env.local
LOOMCYCLE_SQLMEM_PG_DSN=postgres://loomcycle_sqlmem:<strong-secret>@db-host:5432/loomcycle_sqlmem
```

Requirements / notes:

- **The aux DSN must use password authentication.** The admin password is the
  key from which every per-scope role's password is derived
  (`HMAC(admin-password, role-name)`), so the runtime can authenticate as each
  scope role. A passwordless DSN (trust / peer / cert) is refused at startup.
- **`pg_hba.conf` must let the per-scope roles connect.** The runtime opens a
  dedicated connection authenticated as each scope's `LOGIN` role
  (`sqlmem_r_…`); the same host/auth rule that admits the admin role generally
  covers them (e.g. `host loomcycle_sqlmem all <runtime-cidr> scram-sha-256`).
  This is what makes the scope role the agent's `session_user` — the property
  the cross-scope isolation rests on.
- **Leave `PUBLIC`'s `CONNECT` on the aux database in place** (the default). The
  scope roles connect via it; the runtime does **not** grant `CONNECT` per scope
  (that would serialize concurrent first-touches on the shared `pg_database`
  row). The isolation boundary is the per-scope **schema** (`USAGE` is granted
  per scope, never to `PUBLIC`), not DB-level `CONNECT` — so on a *dedicated* aux
  DB, `PUBLIC CONNECT` is safe. (Revoking schema-level `PUBLIC` access, below, is
  still correct + recommended.)
- **PostgreSQL 13+** (the runtime relies only on `LOGIN` roles, `ALTER ROLE …
  SET`, and per-role `search_path`/`statement_timeout` — all long-standing).
- Do **not** install `dblink` / `postgres_fdw` / `file_fdw` / untrusted
  procedural languages in the aux DB — the scope roles can't `CREATE EXTENSION`,
  but not installing them removes the surface entirely.
- **Durable-scope cleanup:** `run` scopes are dropped automatically at run
  completion (schema + role). Durable `agent`/`user` scopes persist (one schema +
  role each) — GC for abandoned durable scopes is a Phase 3 item; today an
  operator reclaims one by `DROP SCHEMA … CASCADE; DROP ROLE …` for the unused
  scope (its name is `sqlmem_s_…` / `sqlmem_r_…`).
- **Backups:** SQL Memory is included in the runtime JSON snapshot — see
  *Snapshot integration* below. A database-level backup of the aux DB / the
  sqlmem root is still recommended for large datasets (the snapshot is built
  in-memory under a 512 MB cap).

## Snapshot integration

`POST /v1/_snapshot` (and the connector / CLI) capture every **durable**
(`agent`/`user`) scope as a tier-tagged **logical dump** — the schema DDL plus
each table's data — into an optional `sqlmem` section of the envelope. Restore
replays it through the normal provisioned path, so a restored scope is identical
to one the runtime created itself. `run` scopes are never snapshotted.

- **Opt-in by configuration:** the section appears only when SQL Memory is
  enabled. A runtime without it produces a byte-identical pre-3e envelope.
- **Same-tier:** a dump is tier-specific (a postgres dump carries per-column
  cast types a sqlite scope can't replay). Restore into a **different** tier is
  **skipped** with a warning (mirrors the embedding snapshot's vector-support
  skip); restore into the **same** tier on any host works (scope identity is a
  deterministic hash, so a postgres scope restores under the same schema name).
- **Idempotent:** a re-restore skips a table that is already non-empty.
- **Postgres identity registry:** because a scope's postgres schema name is a
  one-way hash of `(tenant, scope, scope_id)`, the runtime records that mapping
  in a small `sqlmem_meta.scope_registry` table (created automatically) so a
  capture can name the scope it restores into. sqlite recovers identity from its
  file-path layout — no registry needed.
- **Fidelity (postgres):** tables (columns/`DEFAULT`/`NOT NULL`/stored-generated),
  enum types, owned sequences (serial round-trips — counter restored via
  `setval`), PK/UNIQUE/CHECK/exclusion + standalone indexes, and foreign keys
  (applied after data). Documented non-goals — a scope using one yields a
  per-scope restore **warning**, not silent loss: other user-defined types
  (domains, composite, functions, aggregates, operators), views/triggers,
  `GENERATED ALWAYS AS IDENTITY` (restored as a plain column; values preserved),
  and custom sequence parameters. sqlite captures the verbatim `sqlite_master`
  DDL + data (binary BLOBs survive base64-tagged, integers keep full precision
  and storage class).

For consistency across sections, **pause the runtime** (`POST /v1/runtime/pause`)
before capturing — a scope written between the section reads is otherwise
captured at the read instant.

## Audit

Every op emits an `audit` event (actor tenant/subject, agent, run, scope, op,
rows, duration, error) plus — in `full` mode — the **redacted** statement text
(the F32 secret redactor scrubs it); `metadata` mode omits the statement for
sensitive deployments. Auditing is best-effort and never blocks the op.
