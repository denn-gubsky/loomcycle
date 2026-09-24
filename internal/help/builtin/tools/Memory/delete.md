---
name: Memory/delete
description: "Memory op=delete — remove one key/value entry; reports whether it existed."
---
`delete` removes one entry by key. Deleting a key that does not exist is not
an error — the result just says `deleted: false`. It does not remove a Path
name the entry was given with `set`'s `path`; remove that with `Path op=rm`.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `key` (required) — the entry to remove. There is no delete-by-path; resolve
  a path first if that is all you have.

## Returns

`{deleted}` — `true` when an entry was removed, `false` when there was none.

## Errors

- `delete: missing required field: key`.
- `Memory.delete: core block "persona" ... is read_only` — the operator owns
  that key; you may not delete it.
- `Memory tool: scope "tenant" not in this agent's memory_scopes [...]` — not
  granted; retrying is pointless.

## Examples

Forget a stored preference:

```json
{"op": "delete", "scope": "user", "key": "prefs/voice"}
```

```json result
{"deleted": true}
```

Deleting a key that was never set is harmless:

```json
{"op": "delete", "scope": "agent", "key": "drafts/unused"}
```

```json result
{"deleted": false}
```
