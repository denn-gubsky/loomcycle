---
name: Channel/release
description: "Channel op=release — on a channel declared hold, let the oldest held messages through to readers, one step at a time."
---
A channel declared `hold` stores every publish but delivers none of them. It
is a breakpoint: a workflow wired through that channel stops there. `release`
lets the oldest held messages through (one by default), and readers then see
them as if they had just been published.

The one thing to get right: `release` needs the **publish** allowlist, not
subscribe — letting a message through finishes its publish.

## Arguments

- `channel` (required) — a declared channel your `channels.publish` allowlist
  covers.
- `count` — how many held messages to release, oldest first (default 1, at
  most 1000).

## Returns

`{channel, released: [message ids], released_count, still_held}`. Releasing
when nothing is held returns `released_count: 0` — not an error. On a channel
that is not declared `hold`, the result adds a `note` saying nothing new is
held there.

## Errors

- `release: count N exceeds max 1000` — release in smaller steps.
- `this agent has no publish allowlist` / `publish not allowed on channel` —
  only a change to the agent definition fixes it.
- a refusal naming `publisher: system` or `_system/` — system channels are
  released by the operator, not by agents.

## Examples

Step the workflow forward by one message:

```json
{"op": "release", "channel": "review/drafts"}
```

```json result
{"channel": "review/drafts", "released": ["msg_18a3f2c9b1d40e7f5c2a9b1e"], "released_count": 1, "still_held": 3}
```

Let the next three through at once:

```json
{"op": "release", "channel": "review/drafts", "count": 3}
```
