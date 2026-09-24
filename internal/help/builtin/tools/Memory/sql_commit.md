---
name: Memory/sql_commit
description: "Memory op=sql_commit — commit the innermost open transaction level on one scope's SQL database; depth 0 means the whole transaction is committed."
---
`sql_commit` finishes the transaction you opened with `sql_begin` on the same
`scope`, making its writes permanent. With nested levels it closes only the
**innermost** one: keep committing until `depth` is `0`, or the outer
transaction is still open and its writes are not yet saved.

## Arguments

- `scope` (required) — the same scope you passed to `sql_begin`: `agent`,
  `user`, `tenant` or `run`.

## Returns

`{ok: true, depth}` — the nesting depth after this commit. `0` means the
transaction is committed and closed; later calls auto-commit again.

## Errors

- `sql_commit: no open transaction to commit for this scope` — nothing is
  open on this scope (it may have been rolled back after sitting idle, or you
  began it on a different scope). Your earlier writes in that transaction are
  NOT saved; redo them.
- `sql_commit: an explicit transaction requires an active run`.
- `sql_scopes` / not-enabled refusals as on `sql_exec`.

## Examples

Commit the writes made since `sql_begin` on the user's database:

```json
{"op": "sql_commit", "scope": "user"}
```

```json result
{"ok": true, "depth": 0}
```

Closing a nested savepoint leaves the outer transaction open:

```json
{"op": "sql_commit", "scope": "run"}
```

```json result
{"ok": true, "depth": 1}
```
