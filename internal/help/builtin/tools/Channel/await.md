---
name: Channel/await
description: "Channel op=await — wait until messages are waiting on any, all, or at least N across several channels (or a timeout), without consuming them."
---
`await` is a barrier over channels: it waits until messages are waiting on
**any** of the channels you name, on **all** of them, or **at least `n`** in
total — or until `wait_ms` runs out. Use it to join producers you did not
spawn (scheduled runs, webhooks, other agents). To join sub-agents you spawned
yourself, `Agent` op=parallel_spawn already waits for them.

The one thing to get right: **`await` never commits.** It shows you the
messages and a `next_cursor` per channel; `ack` each channel's cursor once
you have processed it. This makes it the safe way to read: nothing is
consumed until you say so.

## Arguments

- `channels` (required) — 1 to 32 channel names, each covered by your
  `channels.subscribe` allowlist. Duplicates are ignored.
- `mode` — `any` (default: at least one channel has a message), `all` (every
  channel has one), or `at_least` (the total across channels reaches `n`).
- `n` — required for `mode: at_least`, greater than 0.
- `wait_ms` — how long to wait for the condition, in milliseconds. `0` (the
  default) checks once and returns. Capped by the operator.
- `max_messages` — per channel, how many messages to return (default 10, at
  most 100). With `at_least`, only these count toward `n`.
- `from_cursor` — read every channel after this cursor instead of after its
  committed one. Rarely useful with more than one channel.

## Returns

`{satisfied, timed_out, mode, fired, results, total_messages}`. `fired` lists
the channels that had messages. `results` maps each channel name to
`{messages: [{id, value, published_at}], next_cursor}`. A timeout is not an
error: you get `satisfied: false, timed_out: true` and whatever had arrived.

## Errors

- `await: missing required field: channels (non-empty list)`,
  `await: too many channels (N > max 32)`.
- `await: unknown mode ...`, `await: mode=at_least requires n > 0`.
- A channel that is undeclared or not in your subscribe allowlist refuses the
  whole call, naming that channel. Remove it and call again.

## Examples

Wait up to a minute for both workers to report:

```json
{"op": "await", "channels": ["results/worker-a", "results/worker-b"], "mode": "all", "wait_ms": 60000}
```

```json result
{"satisfied": true, "timed_out": false, "mode": "all", "fired": ["results/worker-a", "results/worker-b"], "total_messages": 2,
 "results": {
  "results/worker-a": {"messages": [{"id": "msg_18a3f2c9b1d40e7f5c2a9b1e", "value": {"ok": true}, "published_at": "2026-09-24T14:02:11.482Z"}], "next_cursor": "cur_18a3f2c9b1d40e7f_msg_18a3f2c9b1d40e7f5c2a9b1e"},
  "results/worker-b": {"messages": [{"id": "msg_18a3f2d0a7c3e11b09d4f2aa", "value": {"ok": true}, "published_at": "2026-09-24T14:02:40.107Z"}], "next_cursor": "cur_18a3f2d0a7c3e11b_msg_18a3f2d0a7c3e11b09d4f2aa"}}}
```

Read one queue safely: take up to five items without consuming them, then
`ack` its `next_cursor` when they are done:

```json
{"op": "await", "channels": ["jobs/cv-requests"], "max_messages": 5, "wait_ms": 20000}
```

Wait for any three findings across four researchers:

```json
{"op": "await", "channels": ["findings/alpha", "findings/beta", "findings/gamma", "findings/delta"], "mode": "at_least", "n": 3, "wait_ms": 120000}
```
