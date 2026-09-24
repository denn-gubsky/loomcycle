---
name: Memory/sql_begin
description: "Memory op=sql_begin — open a transaction on one scope's SQL database so the following sql_exec/sql_query calls commit or roll back together; a second begin nests a savepoint."
---
`sql_begin` starts a transaction for one scope. Until you finish it with
`sql_commit` or `sql_rollback`, every `sql_exec` and `sql_query` on **that
same scope** in this run goes through it, so several writes succeed or fail
together. There is no transaction id to pass around — the transaction is
identified by this run and the `scope`. A second `sql_begin` while one is
open nests a savepoint; each result reports the depth.

If you never finish it, it is rolled back when the run ends or after it has
sat idle too long. Do not leave one open across long waits.

## Arguments

- `scope` (required) — `agent`, `user`, `tenant` or `run`. The transaction
  covers only this scope's database; calls on another scope are unaffected.

## Returns

`{ok: true, depth}` — `1` for a new transaction, `2` or more for a nested
savepoint.

## Errors

- `sql_begin: an explicit transaction requires an active run` — not
  available outside a run.
- `sql_begin: transaction nesting depth limit (N) reached — commit or roll
  back a nested level first`.
- `sql_begin: too many open transactions (N) — commit or rollback before
  opening more`.
- `sql_begin: a transaction is being opened for this scope — retry` — retry
  once.
- `sql_scopes` / not-enabled refusals as on `sql_exec`.

## Examples

Open a transaction on the user's database before two related writes:

```json
{"op": "sql_begin", "scope": "user"}
```

```json result
{"ok": true, "depth": 1}
```

Then run your `sql_exec` calls with `"scope": "user"` — for example two
`UPDATE stock ...` statements — and finish with
`{"op": "sql_commit", "scope": "user"}`, or `sql_rollback` if a step failed.

A second begin while one is open marks a savepoint you can undo on its own:

```json
{"op": "sql_begin", "scope": "user"}
```

```json result
{"ok": true, "depth": 2}
```
