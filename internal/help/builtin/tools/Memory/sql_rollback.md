---
name: Memory/sql_rollback
description: "Memory op=sql_rollback — undo the innermost open transaction level on one scope's SQL database; a nested rollback keeps the outer transaction going."
---
`sql_rollback` discards the writes made since the matching `sql_begin` on the
same `scope`. With nested levels it undoes only the **innermost** one — the
outer transaction continues and still needs `sql_commit` or another
`sql_rollback`. Use it when one step of a multi-step change failed.

## Arguments

- `scope` (required) — the same scope you passed to `sql_begin`: `agent`,
  `user`, `tenant` or `run`.

## Returns

`{ok: true, depth}` — the nesting depth after this rollback. `0` means the
whole transaction was rolled back and closed.

## Errors

- `sql_rollback: no open transaction to roll back for this scope` — nothing
  is open on this scope; there is nothing to undo.
- `sql_rollback: an explicit transaction requires an active run`.
- `sql_scopes` / not-enabled refusals as on `sql_exec`.

## Examples

A write failed half-way; undo the whole transaction:

```json
{"op": "sql_rollback", "scope": "user"}
```

```json result
{"ok": true, "depth": 0}
```

Undo only the nested savepoint and keep the outer transaction:

```json
{"op": "sql_rollback", "scope": "agent"}
```

```json result
{"ok": true, "depth": 1}
```
