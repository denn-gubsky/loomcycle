---
name: Channel/broadcast
description: "Channel op=broadcast — publish the same JSON value to up to 32 channels in one call; refused whole if any channel is not publishable."
---
`broadcast` publishes one value to several channels at once — for example,
telling N workers to start. It takes the same `value`, `ttl` and `deliver_at`
as `publish`, applied to every channel.

The one thing to get right: every channel is checked **before anything is
written**. If one channel is undeclared or outside your publish allowlist, the
whole call is refused and nothing is sent.

## Arguments

- `channels` (required) — 1 to 32 channel names, each covered by your
  `channels.publish` allowlist. Duplicates are sent once.
- `value` (required) — the JSON value to send to each channel.
- `ttl` — seconds until each message expires (default: each channel's own
  default).
- `deliver_at` — RFC3339 time from which readers see the messages. See
  `{"op":"help","topic":"Channel/publish"}`.

## Returns

`{published, failed, results}`. `results` has one entry per channel, each
shaped like a `publish` result (`message_id`, `channel`, `dropped_oldest`,
and `visible_at` / `held` when they apply). A storage failure on one channel
shows up as `{channel, error}` in its entry while the others stand; retry
only that channel with `publish`.

## Errors

- `broadcast: missing required field: channels (non-empty list)`,
  `broadcast: too many channels (N > max 32)`.
- `broadcast: missing required field: value`, `broadcast: payload ... exceeds
  max ...`, `broadcast: invalid deliver_at ...`.
- `broadcast: ...` followed by a channel refusal (undeclared, not allowed,
  system channel) — fix or drop that channel; the rest were not sent.

## Examples

Start three workers on the same batch:

```json
{"op": "broadcast", "channels": ["work/worker-a", "work/worker-b", "work/worker-c"], "value": {"batch_id": "b_2026_09_24", "action": "start"}}
```

```json result
{"published": 3, "failed": 0, "results": [
  {"message_id": "msg_18a3f2c9b1d40e7f5c2a9b1e", "channel": "work/worker-a", "dropped_oldest": 0},
  {"message_id": "msg_18a3f2c9b1d40e7f7d11c0e3", "channel": "work/worker-b", "dropped_oldest": 0},
  {"message_id": "msg_18a3f2c9b1d40e7f90ab44f1", "channel": "work/worker-c", "dropped_oldest": 0}]}
```

Announce a shutdown that expires after ten minutes:

```json
{"op": "broadcast", "channels": ["work/worker-a", "work/worker-b"], "value": {"action": "stop"}, "ttl": 600}
```
