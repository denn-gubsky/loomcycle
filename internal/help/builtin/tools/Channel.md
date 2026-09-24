---
name: Channel
description: "Channel tool — a persistent message bus between agents: publish JSON to a named channel, read it with cursors, wait on several channels at once, or fan one message out to many."
---
The `Channel` tool is a **persistent message bus**. You publish a JSON value to
a named channel; whoever reads that channel later gets it, in order, even if
they start after you finish. Messages are stored, not streamed: nothing is lost
because no one was listening at the moment you published.

## When to use it — and when not

- **Use Channel** to hand work or results to agents you did not spawn: a
  worker pool, a scheduled run, a webhook-triggered run, a later run of
  yourself.
- **Do not use Channel** when you need a sub-agent's answer before you go on.
  Call the `Agent` tool instead: it waits and returns the final text. For how
  to choose, read `{"op":"help","topic":"subagents"}` and
  `{"op":"help","topic":"fan-out-patterns"}`.
- **Do not use Channel** to remember something for yourself. That is `Memory`.

## Operations

Every operation takes `op`. Fetch one operation's article, with examples, as
`Channel/<op>` — for example `{"op":"help","topic":"Channel/subscribe"}`.

| op | What it does | Required besides `op` |
|---|---|---|
| `publish` | append one JSON message to a channel | `channel`, `value` |
| `subscribe` | read new messages and **move the cursor past them** | `channel` |
| `ack` | commit a cursor you got from `subscribe` or `await` | `channel`, `cursor` |
| `peek` | read without moving the cursor (from the oldest by default) | `channel` |
| `release` | let the oldest held messages through on a `hold` channel | `channel` |
| `list_channels` | your publish and subscribe allowlists | — |
| `await` | wait until messages arrive on any / all of several channels | `channels` |
| `broadcast` | publish one value to several channels in one call | `channels`, `value` |

## Which channels you may use

A channel must be **declared** (by the operator's config, or created at
runtime as a channel definition) before anyone can use it. On top of that,
your agent definition has two allowlists: `channels.publish` and
`channels.subscribe`. A pattern ending in `/*` matches every channel under that
prefix (`findings/*` matches `findings/alpha`, not `findings`).

- `publish`, `broadcast` and `release` need the **publish** allowlist.
- `subscribe`, `peek`, `ack` and `await` need the **subscribe** allowlist.
- Channels named `_system/...`, or declared `publisher: system`, are
  read-only for agents.

Call `{"op":"list_channels"}` to see your allowlists, and
`Context {"op":"channels"}` to see every declared channel with its scope.

## Scope is fixed by the channel, not by you

**There is no `scope` argument.** Each channel's definition declares its scope,
and every call on that channel uses it:

- `agent` — one stream per agent NAME. Two different agents on an agent-scoped
  channel read and write two different streams.
- `user` — one stream per end-user of the run. Needs a user id on the run.
- `tenant` — one stream shared by everyone in the tenant.
- `global` — one stream shared by every tenant.

An isolated user may use only `agent` and `user` channels. For scopes across
all tools, read `{"op":"help","topic":"scopes"}`.

## Cursors and delivery

Messages are read in order. A **cursor** marks a position in that order; it
looks like `cur_18a3f2c9b1d40e7f_msg_18a3f2c9b1d40e7f5c2a9b1e`, and `cur_0`
means "before the oldest message". Always pass back a cursor you were given;
never build one.

Each channel stream has **one committed cursor**, shared by everyone who reads
that stream — not one per reader. That makes a shared channel a work queue:
when one agent subscribes and gets a message, the next subscriber does not.

- `subscribe` commits the cursor past the batch **before you process it**. If
  your run dies mid-batch, those messages are not re-delivered by a plain
  `subscribe`.
- To process safely, read with `await` (it never commits), do the work, then
  `ack` the `next_cursor` it gave you.
- Messages past their TTL disappear, and a channel with `max_messages` drops
  its oldest message when full (`dropped_oldest` in the publish result).

For operators publishing from outside a run, read
`{"op":"help","topic":"channel-admin"}`; for `_system/` channels and deferred
publishing, `{"op":"help","topic":"system-channels"}`.
