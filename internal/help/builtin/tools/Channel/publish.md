---
name: Channel/publish
description: "Channel op=publish — append one JSON message to a declared channel; optionally delay delivery with deliver_at or shorten its life with ttl."
---
`publish` appends one message to a channel. It returns as soon as the message
is stored; readers pick it up whenever they next read. The one thing to get
right: **`value` is a JSON value, not a string of JSON** — pass
`{"task": "..."}`, not `"{\"task\": ...}"`.

## Arguments

- `channel` (required) — a declared channel your `channels.publish` allowlist
  covers.
- `value` (required) — any JSON value: an object, array, string, number or
  boolean. The operator may cap its size.
- `ttl` — seconds until the message expires. Omit it to use the channel's
  default TTL (which may be none).
- `deliver_at` — an RFC3339 time, e.g. `2026-09-24T15:00:00Z`. The message is
  stored now but readers see it only from that time. A time in the past means
  "now". The TTL still counts from publish time, so size `ttl` to cover the
  delay plus the time you want it readable. Use `Context {"op":"time"}` to get
  the current time.

There is no `scope` argument: the channel's definition fixes it.

## Returns

`{message_id, channel, dropped_oldest}`. `dropped_oldest` greater than 0 means
the channel was full and that many old messages were discarded. A delayed
message also returns `visible_at`. On a channel declared `hold`, the result
has `held: true`: the message is stored but nobody receives it until someone
calls `release`.

## Errors

- `... is not declared in operator config (channels: block)` — the channel
  does not exist. Check the name with `Context {"op":"channels"}`; retrying
  is pointless.
- `this agent has no publish allowlist` / `publish not allowed on channel` —
  your agent may not publish here. Only a change to the agent definition
  fixes it.
- a refusal naming `publisher: system` or the reserved `_system/` prefix —
  agents may never publish to system channels.
- `... has scope=user but the run has no user_id` — this run has no end-user,
  so a user-scoped channel cannot be used.
- `isolated user: access to the shared ... scope is not permitted` — you may
  not use tenant or global channels on this run.
- `publish: missing required field: value`, `publish: payload (N bytes)
  exceeds max M bytes` — fix the value.
- `publish: invalid deliver_at ...` — use a full RFC3339 time with a zone.

## Examples

Hand a work item to a worker channel:

```json
{"op": "publish", "channel": "jobs/cv-requests", "value": {"candidate_id": "c_4471", "role": "backend engineer"}}
```

```json result
{"message_id": "msg_18a3f2c9b1d40e7f5c2a9b1e", "channel": "jobs/cv-requests", "dropped_oldest": 0}
```

A reminder that becomes readable at 15:00 UTC and lives for two hours from
now:

```json
{"op": "publish", "channel": "reminders", "value": {"text": "check the deploy"}, "deliver_at": "2026-09-24T15:00:00Z", "ttl": 7200}
```
