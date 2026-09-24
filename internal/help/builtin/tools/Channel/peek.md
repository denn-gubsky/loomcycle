---
name: Channel/peek
description: "Channel op=peek — look at a channel's messages without consuming them; starts from the oldest message unless you pass from_cursor."
---
`peek` shows messages on a channel without consuming anything: no cursor
moves, so other readers are unaffected. Use it to inspect a queue or re-read
history.

The one thing to get right: **`peek` starts from the OLDEST message** when you
omit `from_cursor` — not from where the committed cursor stands. So it can show
messages that `subscribe` would no longer return. It also returns no cursor;
to read-then-commit, use `await` and `ack`.

## Arguments

- `channel` (required) — needs the `channels.subscribe` allowlist.
- `max_messages` — how many to return (default 10, at most 100).
- `from_cursor` — show messages after this cursor (one you were given by
  `subscribe` or `await`). Omitted or `"cur_0"` = from the oldest.

## Returns

`{channel, messages: [{id, value, published_at}]}`, oldest first. Expired
messages, held messages and messages whose `deliver_at` is still in the future
are not shown.

## Errors

- An empty list is not an error: nothing readable is stored in this scope's
  stream.
- `peek: invalid channel cursor ...` — `from_cursor` was not a cursor the
  tool gave you.
- `... is not declared`, `subscribe not allowed on channel` — the channel is
  unknown or closed to you; retrying is pointless.

## Examples

Look at the first 20 messages waiting on a queue:

```json
{"op": "peek", "channel": "jobs/cv-requests", "max_messages": 20}
```

```json result
{"channel": "jobs/cv-requests", "messages": [
  {"id": "msg_18a3f2c9b1d40e7f5c2a9b1e", "value": {"candidate_id": "c_4471"}, "published_at": "2026-09-24T14:02:11.482Z"}]}
```

Look only at what came after a cursor you already have:

```json
{"op": "peek", "channel": "jobs/cv-requests", "from_cursor": "cur_18a3f2c9b1d40e7f_msg_18a3f2c9b1d40e7f5c2a9b1e"}
```
