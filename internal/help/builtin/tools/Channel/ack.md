---
name: Channel/ack
description: "Channel op=ack — commit a cursor on a channel after you have finished processing the messages before it, so the next subscribe starts after them."
---
`ack` commits a cursor: it tells the channel "everything up to here is done".
The next `subscribe` on that channel starts after it. Use it with `await`,
which reads without committing: read, do the work, then ack the cursor the
read gave you. That way a run that dies mid-task leaves the messages in place
for the next reader.

The one thing to get right: **pass the `next_cursor` you were given**, not a
message `id`. A message id (`msg_...`) is not a cursor (`cur_...`).

## Arguments

- `channel` (required) — the channel the cursor came from. Needs the
  `channels.subscribe` allowlist.
- `cursor` (required) — a `next_cursor` from `subscribe`, or from one
  channel's entry in an `await` result.

## Returns

`{ok: true}`. Acking the cursor that is already committed is a harmless
no-op — including the `next_cursor` `subscribe` returned, which it has already
committed.

## Errors

- `ack: missing required field: cursor`.
- `ack: channel: ack cursor older than committed` — someone (another reader
  of the same stream, or an earlier `subscribe`) already moved past this
  point. Nothing to do: those messages are already consumed. Do not retry.
- `ack: invalid channel cursor ...` — the value is not a cursor the tool gave
  you, for example a message id.
- `... is not declared`, `subscribe not allowed on channel` — the channel is
  unknown or closed to you.

## Examples

Commit after processing what `await` returned for `jobs/cv-requests`:

```json
{"op": "ack", "channel": "jobs/cv-requests", "cursor": "cur_18a3f2c9b1d40e7f_msg_18a3f2c9b1d40e7f5c2a9b1e"}
```

```json result
{"ok": true}
```
