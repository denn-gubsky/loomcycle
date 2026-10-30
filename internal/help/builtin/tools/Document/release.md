---
name: Document/release
description: "Document op=release — end a scope lease this run holds."
---
`release` ends a lease taken with `lease`, so the next pass need not wait for
it to expire. Releasing a lease this run does not hold is not an error: the
answer says `released: false`. That happens when the lease expired and another
pass took it over, or when it was never held. Release on every exit path of a
pass, the failing ones included.

## Arguments

- `name` (required) — the lease's name.
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{name, released}`.

## Errors

- `release: missing required field: name`.
- A permission error for a caller that may not hold a lease.

## Examples

```json
{"op": "release", "name": "linker"}
```
