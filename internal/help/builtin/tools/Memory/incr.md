---
name: Memory/incr
description: "Memory op=incr — atomically add an integer to a numeric entry (a counter); a missing key starts at the delta."
---
`incr` adds `delta` to the number stored at a key and returns the new value,
atomically — two runs incrementing at once never lose a count. Use it instead
of get, add, set. A key that does not exist yet is created with the value
`delta`. **The existing value must be a JSON number.**

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `key` (required) — the counter's key.
- `delta` — an integer to add (default 1; may be negative).
- `ttl` — seconds until expiry; sets or resets it. Omit to keep the current
  expiry.

## Returns

`{value}` — the number after the increment.

## Errors

- `incr: existing value is not a JSON number — use set with a number, or
  delete first` — the key holds text or an object. Retrying is pointless.
- `incr: missing required field: key`.
- `Memory.incr: core block ... is read_only` — the operator owns that key.

## Examples

Count one more completed report:

```json
{"op": "incr", "scope": "agent", "key": "stats/reports_written"}
```

```json result
{"value": 42}
```

Take back three credits from the user's balance:

```json
{"op": "incr", "scope": "user", "key": "credits/remaining", "delta": -3}
```
