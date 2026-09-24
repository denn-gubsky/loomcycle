---
name: Memory/sql_exec
description: "Memory op=sql_exec — run ONE DDL/DML statement (CREATE, DROP, ALTER, INSERT, UPDATE, DELETE, REPLACE) against this scope's SQL database."
---
`sql_exec` changes your per-scope SQL database: create a table, insert,
update or delete rows. One statement per call; each call commits on its own
unless you opened a transaction with `sql_begin`. SQL needs its own grant,
**`sql_scopes`**. Use `?` placeholders with `args` for values — never paste
user text into the statement.

## Arguments

- `scope` (required) — `agent`, `user`, `tenant`, or `run` (a scratch
  database dropped when the run ends). Each scope is a different database; a
  table created in `run` is not visible in `user`.
- `statement` (required) — ONE statement starting with `CREATE`, `DROP`,
  `ALTER`, `INSERT`, `UPDATE`, `DELETE`, `REPLACE` or `WITH` (a CTE ending in
  a write). `SELECT`, `ATTACH`, `PRAGMA`, `VACUUM`, `load_extension`,
  `BEGIN`/`COMMIT`/`SAVEPOINT` and multiple statements are refused.
- `args` — positional values for the `?` placeholders. `{"$embed": "text"}`
  stores that text's embedding (PostgreSQL tier with pgvector only).
- `timeout_ms` — accepted but ignored.

## Returns

`{rows_affected, last_insert_id}`. `last_insert_id` is filled on the SQLite
tier only; on PostgreSQL it is 0.

## Errors

- `Memory tool: this agent has no sql_scopes configured ...` / `sql scope ...
  not in this agent's sql_scopes` — not granted; retrying is pointless.
- `SQL Memory is not enabled on this server ...`.
- `sql_exec: sql_exec only runs DDL/DML (...); "select" is not allowed` — use
  `sql_query` to read.
- `sql_exec: explicit transactions are not supported ...` — do not send
  `BEGIN`; use `sql_begin` / `sql_commit` instead.
- `sql_exec: sqlmem: scope is at its quota (...) — delete rows or drop tables
  before writing`.
- Database errors (duplicate key, no such column, …) come back wrapped as
  `sql_exec: ...`; fix the statement.

## Examples

Create a table once:

```json
{"op": "sql_exec", "scope": "user", "statement": "CREATE TABLE IF NOT EXISTS tasks (id INTEGER PRIMARY KEY, title TEXT NOT NULL, priority INTEGER DEFAULT 0, done INTEGER DEFAULT 0)"}
```

```json result
{"rows_affected": 0, "last_insert_id": 0}
```

Insert a row with bound values:

```json
{"op": "sql_exec", "scope": "user", "statement": "INSERT INTO tasks (title, priority) VALUES (?, ?)", "args": ["Renew passport", 3]}
```

```json result
{"rows_affected": 1, "last_insert_id": 7}
```

Scratch table for this run only:

```json
{"op": "sql_exec", "scope": "run", "statement": "CREATE TABLE findings (id INTEGER PRIMARY KEY, source TEXT, note TEXT)"}
```
