---
name: Memory/merge
description: "Memory op=merge — atomically deep-merge a JSON object's fields into the object stored at a key, so concurrent updates to different fields never overwrite each other."
---
`merge` overlays the fields of `value` onto the JSON object stored at `key`,
in one atomic step. Use it instead of get, modify, set when another run may
update the same object: two merges of different fields both survive. Nested
objects merge field by field; any other value (array, string, number, `null`)
**replaces** what was at that field. A missing key starts from `{}`.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `key` (required) — the object's key.
- `value` (required) — a JSON **object**. Arrays and scalars are refused.
- `ttl` — seconds until expiry. Omit for no expiry.

## Returns

`{value}` — the whole object after the merge.

## Errors

- `merge: value must be a JSON object` — wrap the fields in `{...}`.
- `merge: existing value is not a JSON object (use set to overwrite)` — the
  key holds an array or scalar. Retrying is pointless; use `set`.
- `merge: merged value (N bytes) exceeds max M bytes`, or a size-cap refusal
  (`Memory.merge: … quota …` / `… limit_bytes …`). Nothing was written — the
  object is too big; store less.
- `Memory.merge: core block ... is read_only` — the operator owns that key.

## Examples

Record the user's timezone without touching the rest of their profile:

```json
{"op": "merge", "scope": "user", "key": "profile", "value": {"timezone": "Europe/Kyiv"}}
```

```json result
{"value": {"name": "Olena", "timezone": "Europe/Kyiv", "prefs": {"tone": "concise"}}}
```

Change one nested field; its siblings stay:

```json
{"op": "merge", "scope": "user", "key": "profile", "value": {"prefs": {"language": "uk"}}}
```

```json result
{"value": {"name": "Olena", "timezone": "Europe/Kyiv", "prefs": {"tone": "concise", "language": "uk"}}}
```
