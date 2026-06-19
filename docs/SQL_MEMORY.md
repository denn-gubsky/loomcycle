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

`LOOMCYCLE_SQLMEM_PG_DSN` carries the aux credentials — like `LOOMCYCLE_PG_DSN`
it is on the env-expand denylist and is **never** interpolatable into a
`${...}` config/MCP field.

## Postgres tier — security model

Isolation is **engine-enforced least privilege** (the parsed-statement validator
is defense-in-depth):

- The aux DB is a **different database** from the main loomcycle store, so even a
  hypothetical escape can't reach sessions/runs/tokens/memory.
- Each scope lazily provisions a **schema** + a **`NOLOGIN NOINHERIT` role with
  `USAGE` only on that schema** (`PUBLIC` revoked).
- The runtime connects as the operator-provisioned **non-superuser** admin and,
  per statement, opens a transaction that pins `statement_timeout` + `search_path`
  and then `SET LOCAL ROLE`s down to the scope role before running the agent's
  statement. So the agent runs as a role that **cannot**:
  - reach another scope's schema — no `USAGE`, even with a fully-qualified
    `other_schema.table` reference (engine: *permission denied for schema*);
  - read host files / run programs — `COPY … TO/FROM PROGRAM`, `COPY … '/file'`,
    `pg_read_file`, `pg_ls_dir`, `lo_import`/`lo_export` are all denied;
  - load code or connect out — `CREATE EXTENSION` / `CREATE LANGUAGE` / `dblink`
    are denied;
  - escalate — it is not a member of any privileged role, so `SET ROLE` to one
    fails.
- `sql_query` additionally runs in a **read-only transaction**, so any write the
  validator missed (e.g. `SELECT … INTO`) fails at the engine.
- The schema-size quota uses a `pg_total_relation_size` sweep over the scope
  schema before a write.

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
```

Then point the runtime at it (keep the DSN in `.env.local`, not yaml):

```bash
# .env.local
LOOMCYCLE_SQLMEM_PG_DSN=postgres://loomcycle_sqlmem:<strong-secret>@db-host:5432/loomcycle_sqlmem
```

Requirements / notes:

- **PostgreSQL 16+** is recommended (the runtime uses `GRANT … WITH SET` to let
  the admin `SET ROLE` into a scope role; it falls back to the pre-16 grant form
  on older servers).
- Do **not** install `dblink` / `postgres_fdw` / `file_fdw` / untrusted
  procedural languages in the aux DB — the scope roles can't `CREATE EXTENSION`,
  but not installing them removes the surface entirely.
- **Durable-scope cleanup:** `run` scopes are dropped automatically at run
  completion (schema + role). Durable `agent`/`user` scopes persist (one schema +
  role each) — GC for abandoned durable scopes is a Phase 3 item; today an
  operator reclaims one by `DROP SCHEMA … CASCADE; DROP ROLE …` for the unused
  scope (its name is `sqlmem_s_…` / `sqlmem_r_…`).
- **Backups** of SQL Memory data are taken at the database level (the JSON
  snapshot covers only the main store).

## Audit

Every op emits an `audit` event (actor tenant/subject, agent, run, scope, op,
rows, duration, error) plus — in `full` mode — the **redacted** statement text
(the F32 secret redactor scrubs it); `metadata` mode omits the statement for
sensitive deployments. Auditing is best-effort and never blocks the op.
