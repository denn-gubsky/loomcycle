---
name: Memory/bounded_list
description: "Memory op=bounded_list — atomically append an item to the JSON array at a key and keep only the newest N items (a rolling log)."
---
`bounded_list` appends `value` to the JSON array at `key`, then drops the
oldest items so at most `limit` remain. Use it for a recent-activity log or a
sliding window. It does not deduplicate — every call appends. **`limit` is
required on every call**, not just the first. A missing key starts from `[]`.

## Arguments

- `scope` (required) — `agent`, `user` or `tenant`.
- `key` (required) — the array's key.
- `value` (required) — the ONE item to append (any JSON).
- `limit` (required) — how many of the newest items to keep, 1 to 10000.
- `ttl` — seconds until expiry. Omit for no expiry.

## Returns

`{dropped, value}` — how many old items were trimmed by this call, and the
array after, oldest first.

## Errors

- `bounded_list: limit must be >= 1` — you left `limit` out; add it.
- `bounded_list: limit must be <= 10000`.
- `bounded_list: existing value is not a JSON array (use set to overwrite)`.
- `bounded_list: array (N bytes) exceeds max M bytes`, or a size-cap refusal
  (`Memory.bounded_list: … quota …` / `… limit_bytes …`). Nothing was written —
  lower `limit` or store smaller items.
- `Memory.bounded_list: core block ... is read_only`.

## Examples

Log a finished task, keeping the last 20:

```json
{"op": "bounded_list", "scope": "agent", "key": "log/recent_tasks", "value": {"task": "weekly report", "at": "2026-03-14T09:00:00Z"}, "limit": 20}
```

```json result
{"dropped": 0, "value": [{"task": "invoice check", "at": "2026-03-13T16:20:00Z"}, {"task": "weekly report", "at": "2026-03-14T09:00:00Z"}]}
```

Remember the user's five most recent searches:

```json
{"op": "bounded_list", "scope": "user", "key": "recent_searches", "value": "flights to Lisbon", "limit": 5}
```
