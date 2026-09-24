---
name: Memory/append_dedupe
description: "Memory op=append_dedupe — atomically add an item to the JSON array at a key unless an equal item is already there (a set-like list)."
---
`append_dedupe` adds `value` to the end of the JSON array stored at `key` —
unless an equal item is already in it, in which case nothing changes and
`appended` is `false`. Equality is JSON equality, so `{"a":1,"b":2}` equals
`{"b":2,"a":1}`. It is atomic: two runs adding the same item at once produce
one entry. A missing key starts from `[]`.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `key` (required) — the array's key.
- `value` (required) — the ONE item to add (any JSON). To add several, make
  several calls; an array here is added as a single item.
- `ttl` — seconds until expiry. Omit for no expiry.

## Returns

`{appended, value}` — whether the item was new, and the whole array after.

## Errors

- `append_dedupe: existing value is not a JSON array (use set to overwrite)`
  — the key holds something else. Retrying is pointless.
- `append_dedupe: value is not valid JSON` — quote strings.
- `append_dedupe: array (N bytes) exceeds max M bytes`, or a size-cap refusal
  (`Memory.append_dedupe: … quota …` / `… limit_bytes …`). Nothing was written —
  the list is too big; remove items with `set` or `delete`.
- `Memory.append_dedupe: core block ... is read_only`.

## Examples

Note a topic the user asked about, once:

```json
{"op": "append_dedupe", "scope": "user", "key": "interests", "value": "kubernetes"}
```

```json result
{"appended": true, "value": ["rust", "kubernetes"]}
```

Adding it again changes nothing:

```json
{"op": "append_dedupe", "scope": "user", "key": "interests", "value": "kubernetes"}
```

```json result
{"appended": false, "value": ["rust", "kubernetes"]}
```
