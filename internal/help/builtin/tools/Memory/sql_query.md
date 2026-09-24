---
name: Memory/sql_query
description: "Memory op=sql_query — run ONE read-only SELECT against this scope's SQL database, with ? bind parameters; returns columns and rows."
---
`sql_query` reads from your per-scope SQL database: one `SELECT` (or
`WITH … SELECT`) per call. The database is separate from key/value memory —
tables you create with `sql_exec` live here, not under Memory keys. SQL needs
its own grant, **`sql_scopes`**; having Memory alone does not give it. Pass
values through `args` and `?` placeholders rather than pasting them into the
statement.

## Arguments

- `scope` (required) — `agent`, `user`, `tenant`, or `run` (a scratch
  database shared by this run and its sub-agents, dropped when the run ends).
  Each scope is a different database.
- `statement` (required) — ONE read-only statement. Writes, `ATTACH`,
  `PRAGMA`, `load_extension`, `BEGIN`/`COMMIT` and a second statement after
  `;` are refused.
- `args` — positional values for the `?` placeholders, in order. An element
  `{"$embed": "some text"}` is replaced by that text's embedding, for vector
  search with a `?::vector` cast (needs the PostgreSQL tier with pgvector and
  an embedder).
- `timeout_ms` — accepted but ignored; the server's timeout applies.

If you opened a transaction on this scope with `sql_begin`, the query runs
inside it and sees its uncommitted writes.

## Returns

`{columns, rows, truncated}`. `rows` is an array of arrays, one value per
column, in `columns` order. `truncated: true` means the server's row cap cut
the result — add `LIMIT`/`WHERE` or page with `OFFSET`.

## Errors

- `Memory tool: this agent has no sql_scopes configured ...` or `sql scope
  "tenant" not in this agent's sql_scopes [...]` — not granted. Retrying is
  pointless; only the operator can grant it.
- `SQL Memory is not enabled on this server ...` — the feature is off here.
- `sql_query: sql_query only runs read-only statements (SELECT / WITH …
  SELECT); use sql_exec for writes`.
- `sql_query: only one SQL statement per call is allowed ...` — split it.
- `sql_query: no such table: ...` (wording depends on the database) — create
  it with `sql_exec`, and check you are in the same `scope`.
- `sql_query: $embed requires ...` — vector search is not available here.

## Examples

Open tasks, highest priority first:

```json
{"op": "sql_query", "scope": "user", "statement": "SELECT id, title, priority FROM tasks WHERE done = ? ORDER BY priority DESC LIMIT 10", "args": [0]}
```

```json result
{"columns": ["id", "title", "priority"], "rows": [[7, "Renew passport", 3], [2, "Book dentist", 1]], "truncated": false}
```

Read back scratch results collected earlier in this run:

```json
{"op": "sql_query", "scope": "run", "statement": "SELECT source, count(*) AS n FROM findings GROUP BY source"}
```

Nearest notes by meaning (PostgreSQL tier with vectors):

```json
{"op": "sql_query", "scope": "agent", "statement": "SELECT id, note FROM notes ORDER BY embedding <=> ?::vector LIMIT 5", "args": [{"$embed": "flaky integration tests"}]}
```
