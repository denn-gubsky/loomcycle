---
name: Document/lease
description: "Document op=lease — take a named, expiring lease on this scope, so only one pass of a kind works on it at a time."
---
`lease` makes a pass over a scope exclusive. A links pass takes the `linker`
lease before it reads or writes anything. A second pass of the same kind on the
same scope finds the lease held and stops without writing. Leases with
different names never block each other, so a links pass does not stall memory
consolidation of the same scope.

The lease expires on its own after `ttl_ms`, so a pass that dies never wedges
the scope. The holder renews it by calling `lease` again with the same name,
and ends it with `release`. The holder is the run that took it; a caller cannot
name another holder.

Only the operator, or a driver agent granted statement methods, may hold a
lease.

## Arguments

- `name` (required) — the lease's name, e.g. `linker`: 1–64 characters of
  a-z, 0-9, `_`, `.`, `:`, `/` and `-`.
- `ttl_ms` — how long to hold it, in milliseconds (default 60000, capped at one
  hour).
- `scope` — `user` (default), `agent` or `tenant`.

## Returns

`{name, acquired, expires_at, held_by?}`. When `acquired` is false, `held_by`
names the current holder and `expires_at` says when its lease runs out.

## Errors

- `lease: missing required field: name`.
- `lease: name must be 1–64 characters of a-z 0-9 _ . : / -`.
- `lease: names starting with "principal:" are reserved for the runtime` —
  pick another name.
- `lease: ttl_ms must not be negative` — omit it for the default.
- `lease: a scope lease is held only by a statement-maintaining driver or the
  operator` — a model-driven agent does not take leases.

## Examples

```json
{"op": "lease", "name": "linker", "ttl_ms": 300000}
```

```json result
{"name": "linker", "acquired": true, "expires_at": 1772409900000000000}
```

The same lease on the tenant scope, found held by another pass — stop without
writing, and try again after `expires_at`:

```json
{"op": "lease", "name": "linker", "scope": "tenant"}
```

```json result
{"name": "linker", "acquired": false, "expires_at": 1772409660000000000, "held_by": "run-9f2c"}
```
