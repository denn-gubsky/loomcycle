---
name: Channel/subscribe
description: "Channel op=subscribe — read the next messages after the committed cursor and commit past them; optionally long-poll with wait_ms until something arrives."
---
`subscribe` returns the next messages on a channel and **commits the cursor
past them before it returns**. Calling it again gives you the messages after
those. It is the simple way to drain a queue.

The one thing to get right: because the cursor is committed on return, a
message you received but did not finish processing is **not** handed to you
again by the next `subscribe`. If losing it would matter, read with
`{"op":"help","topic":"Channel/await"}` instead and `ack` when done.

## Arguments

- `channel` (required) — a declared channel your `channels.subscribe`
  allowlist covers.
- `max_messages` — batch size (default 10, at most 100).
- `wait_ms` — if nothing is waiting, wait up to this many milliseconds for a
  message to arrive. `0` (the default) returns at once. The operator caps
  the wait; asking for more is quietly shortened to the cap, and long-poll may
  be disabled entirely.
- `from_cursor` — read after this cursor instead of after the committed one.
  `"cur_0"` replays from the oldest message. Replaying never moves the
  committed cursor backwards.

## Returns

`{channel, messages: [{id, value, published_at}], next_cursor}`, oldest
first. `next_cursor` is the position after the last message returned, and it
is already committed. An empty `messages` list with an empty `next_cursor`
means nothing new was waiting (or the wait ran out).

## Errors

- An empty batch is not an error. If you expected messages, check the
  channel's scope with `Context {"op":"channels"}`: on an `agent`-scoped
  channel you read your own agent's stream, not the publisher's.
- `... is not declared in operator config`, `this agent has no subscribe
  allowlist`, `subscribe not allowed on channel` — the channel is unknown or
  closed to you; retrying is pointless.
- `... has scope=user but the run has no user_id` — this run has no end-user.
- `invalid channel cursor ...` — `from_cursor` was not a cursor the tool gave
  you. Drop it, or use `"cur_0"`.

## Examples

Take up to five work items, waiting up to 20 seconds if the queue is empty:

```json
{"op": "subscribe", "channel": "jobs/cv-requests", "max_messages": 5, "wait_ms": 20000}
```

```json result
{"channel": "jobs/cv-requests", "messages": [
  {"id": "msg_18a3f2c9b1d40e7f5c2a9b1e", "value": {"candidate_id": "c_4471"}, "published_at": "2026-09-24T14:02:11.482Z"}],
 "next_cursor": "cur_18a3f2c9b1d40e7f_msg_18a3f2c9b1d40e7f5c2a9b1e"}
```

Re-read the whole channel from the beginning:

```json
{"op": "subscribe", "channel": "findings/alpha", "from_cursor": "cur_0", "max_messages": 100}
```
