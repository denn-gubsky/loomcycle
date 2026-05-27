---
name: memory-reducers
description: Atomic reducer ops on the Memory tool — merge / append_dedupe / bounded_list. When to use them instead of get-modify-set, and what each guarantees under concurrent calls (v0.12.x).
---
The v0.12.x substrate adds three atomic reducer ops to the Memory
tool. Each is a single round-trip that combines READ + DERIVE +
WRITE under a per-row lock — concurrent calls on the same key
serialise cleanly instead of fighting through a get-modify-set race
where one agent's update silently overwrites another's.

Use these instead of the get-modify-set pattern whenever **two or
more callers can touch the same key**. The classic motivating
scenarios:

- A multi-agent fan-out where each child merges its findings into
  one shared profile key.
- An event log that two agents both `append` to simultaneously.
- A "recent activity" buffer that should hold the last N items but
  multiple callers add to it.

The three ops share a single store primitive (`MemoryAtomicUpdate`)
that holds a per-row lock for the duration of the read-modify-write.
Different keys still run in parallel; only callers touching the
same `(scope, scope_id, key)` triple block each other.

## merge — deep-merge a JSON object

```jsonc
{
  "op": "merge",
  "scope": "user",
  "key": "profile",
  "value": { "likes": ["jazz"], "age": 30 }
}
```

The existing value MUST be a JSON object (or absent — treated as
empty object). The incoming `value` MUST also be a JSON object.
Fields in `value` overlay the existing fields; **nested objects
recurse**; arrays and scalars at any level **replace** the existing
value at that path (no implicit concat).

| Existing | Incoming | Result |
|---|---|---|
| (none) | `{"name":"Alice"}` | `{"name":"Alice"}` |
| `{"name":"Alice","likes":["rock"]}` | `{"likes":["jazz"]}` | `{"name":"Alice","likes":["jazz"]}` |
| `{"a":{"x":1}}` | `{"a":{"y":2}}` | `{"a":{"x":1,"y":2}}` |
| `{"a":{"x":1}}` | `{"a":42}` | `{"a":42}` (non-object overrides nested) |

`merge` REFUSES (with `is_error: true`) when:

- Existing value isn't a JSON object — silent replacement would
  surprise the agent. Use `set` to overwrite explicitly.
- Incoming value isn't a JSON object — `merge` is object-shaped by
  design. Use `append_dedupe` / `bounded_list` for array-shaped
  reducers.

## append_dedupe — idempotent append to an array

```jsonc
{
  "op": "append_dedupe",
  "scope": "user",
  "key": "seen",
  "value": "article-42"
}
```

The existing value MUST be a JSON array (or absent — treated as
empty array). The incoming `value` can be any JSON value. If the
value is already in the array (by JSON-equality), the call is a
no-op and the response is `{"appended": false, ...}`.

JSON-equality is **structural**: `{"a":1,"b":2}` and `{"b":2,"a":1}`
count as equal even though their byte representations differ. Arrays
compare element-wise in order.

The response shape:

```jsonc
{"appended": true,  "value": ["a", "b", "article-42"]}
{"appended": false, "value": ["a", "b", "article-42"]}  // second call with same value
```

Use this when you want "set semantics for an array" — track a
deduplicated history of events the agent has seen / decisions it
has made / users it has talked to.

## bounded_list — append + drop-oldest

```jsonc
{
  "op": "bounded_list",
  "scope": "agent",
  "key": "recent",
  "value": { "event": "user-login", "at": "2026-05-27T10:00:00Z" },
  "limit": 100
}
```

The existing value MUST be a JSON array (or absent — treated as
empty array). Appends `value` to the back of the array; if length
exceeds `limit`, drops from the FRONT until length equals `limit`.

Returns `{"dropped": N, "value": [...]}` where N is the count of
items removed by the trim — informative for agents tracking turnover.

`limit` must be in `[1, 10000]`. The upper bound exists so one
bounded_list call can't blow past a model's context window when the
agent later reads the row.

Insertion order is preserved. **No dedupe** — every call appends.
For dedupe + bounded behaviour together, combine `append_dedupe`
with periodic `delete` + `bounded_list` rotation (or set a TTL).

## Picking between the three

| You want… | Op |
|---|---|
| Update one or more fields of an object without disturbing the others | `merge` |
| Track a set-like collection where duplicates don't count | `append_dedupe` |
| Track a sliding-window list of the N most recent items | `bounded_list` |
| Atomically count something | `incr` (the existing op) |
| Replace the entire value | `set` (the existing op) |

## Concurrency guarantees

**All three ops are atomic.** Two agents calling the same op on the
same key simultaneously produce a deterministic outcome — the
second caller sees the first's update and applies its reducer on
top.

Under the hood:

- **Postgres**: `pg_advisory_xact_lock(hashtextextended(scope:id:key))`
  serialises updates on the SAME key only. Different keys run in
  parallel.
- **SQLite**: `BEGIN IMMEDIATE` serialises ALL writes (SQLite is
  single-writer anyway). Coarser than Postgres but
  engine-appropriate.

This means you SHOULD use these ops when:

- You can't guarantee only one caller touches a key at a time.
- The reducer logic is what the agent intends (merge fields, dedupe
  appends, bounded-buffer) — composing it from `get` + `set` would
  introduce a TOCTOU race.

You DON'T need these ops when:

- The key is per-agent-per-run (no other caller exists).
- You're doing one-shot writes (`set` is fine).
- You're reading without modifying (`get`).

## What the reducer ops do NOT do

- **No cross-key transactions.** Each op is single-key. If you need
  atomicity across two keys, serialise the work behind a Channel
  with one consumer.
- **No conflict resolution callbacks.** The merge / dedupe /
  bounded-list policies are fixed — there's no "if both sides
  changed this field, run my custom merger" hook. If you need
  that, do `get` + agent-side decision + `set` and accept the
  TOCTOU window.
- **No batch reducers.** One call = one update. Bulk loading
  through a single `set` is fastest when the agent owns the full
  value.
- **No history / undo.** The reducer commits in place. To preserve
  history, use `bounded_list` with the operation's *result* (not
  its input) as the appended item.

## See also

- `Context.help(topic="vector-memory")` — when to add `embed: true`
  on top of any reducer op so the row participates in similarity
  search alongside the structural updates.
- `Context.help(topic="scopes")` — how `agent` / `user` / `global`
  resolve `scope_id` server-side. The reducers respect the same
  scoping invariants as `get` / `set`.
- `Context.help(topic="fan-out-patterns")` — the multi-agent
  fan-out scenarios where these reducers prevent lost-updates that
  the previous get-modify-set pattern produced.
