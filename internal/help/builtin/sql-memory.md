---
name: sql-memory
description: A per-scope SQL database on the Memory tool — when to use it, the ops, the capability gate, transactions, vectors, and shared schemas.
---
SQL Memory is a third facet of the `Memory` tool (alongside k/v and the
semantic layer): a real, per-scope SQL database the runtime hosts, isolated
from the main store. Reach for it when an agent needs **related tables, joins,
and aggregates** — structured storage the k/v + vector Memory can't give —
without `Bash + sqlite3` (which is not a sandbox). Each scope's data is
reachable only by that scope; the threat model is escaping the scope, not SQL
injection (the agent is authorized to run SQL).

## Ops (all on the `Memory` tool)
- `op: "sql_exec"` — ONE DDL/DML statement (`CREATE TABLE`/`INSERT`/`UPDATE`/
  `DELETE`/…); `args` are positional bind params for `?`/`$1`. Returns
  `{rows_affected, last_insert_id?}` (`last_insert_id` is sqlite-only — use
  `RETURNING` on postgres).
- `op: "sql_query"` — ONE read-only `SELECT` / `WITH … SELECT`. Returns
  `{columns, rows, truncated}`.
- `op: "sql_begin"` / `"sql_commit"` / `"sql_rollback"` — explicit transactions
  (below).

One statement per call. `ATTACH`, `PRAGMA`, `load_extension`, `COPY`, `SET`,
raw `BEGIN`/`COMMIT`/`SAVEPOINT`, and multiple statements are refused.

## Capability gate (default-deny)
SQL is OFF unless **both** hold: the operator enabled the subsystem
(`sqlmem_enabled`) AND the agent declares `sql_scopes` — having `Memory` in
`tools` is NOT enough. SQL is a distinct capability of the tool, gated
separately:
```yaml
agents:
  research-bot:
    tools: [Memory]
    sql_scopes: [agent, run]   # closed enum {agent,user,run}; empty => every SQL op refuses
```

## Scope = which database
`scope` selects the database: `agent` (this agent, durable, cross-run),
`user` (this end-user, durable, cross-agent), or `run` (ephemeral scratch,
dropped at run completion). Durable scopes are tenant-keyed. See `op=help
topic=scopes`.

## Transactions (atomic multi-step writes)
`sql_begin` opens a transaction for the scope; subsequent `sql_exec`/`sql_query`
on that scope run ON it until `sql_commit` / `sql_rollback`. A txn auto-rolls
back if the run ends or it is abandoned. A second `sql_begin` while one is open
**nests** a SAVEPOINT — then `sql_commit`/`sql_rollback` affect the innermost
level (a nested rollback undoes only that level; the outer txn continues). Each
result reports the current `depth` (1 = the root transaction, 0 = closed).

```jsonc
{"op":"sql_begin",  "scope":"agent"}                                   // depth 1
{"op":"sql_exec",   "scope":"agent", "statement":"INSERT INTO log VALUES ('a')"}
{"op":"sql_begin",  "scope":"agent"}                                   // depth 2 (savepoint)
{"op":"sql_exec",   "scope":"agent", "statement":"INSERT INTO log VALUES ('b')"}
{"op":"sql_rollback","scope":"agent"}    // undoes only 'b'; depth -> 1
{"op":"sql_commit", "scope":"agent"}     // commits 'a'; depth -> 0
```

## Vector columns (postgres tier)
With pgvector installed, a scope can keep embeddings in its own tables and do
semantic KNN + structured filters in one query. Pass a bind arg of the form
`{"$embed": "some text"}` and the runtime embeds it server-side (no raw vector
in your context); reference it with `::vector`:
`... ORDER BY embedding <=> ?::vector`.

## Read-only shared schemas (postgres tier)
The operator can expose curated reference tables (lookups, taxonomies) to every
agent via `sqlmem_shared_schemas`. They are on your `search_path`, so
`SELECT … JOIN countries …` just works — but writes are denied (read-only).
Qualify (`refdata.countries`) to bypass a same-named scope table.

## Bounds
Per-statement timeout, `sql_query` row cap, and an optional per-scope byte
quota apply; durable scopes can be reclaimed by an opt-in TTL / size-budget GC.
The tier follows the main store: **sqlite** = one file per scope (compact,
single-replica); **postgres** = one schema per scope in a separate aux DB,
isolated by a per-scope least-privilege role (multi-replica). Full operator
detail (provisioning, GC, snapshots, security model) is in `docs/SQL_MEMORY.md`.
