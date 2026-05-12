---
name: system-channels
description: The _system/* prefix, publisher:system, the admin endpoint, and deferred publish.
---
v0.8.6 added a separate namespace for loomcycle-authoritative
signals: channels whose name starts with `_system/`. Three things
are different about them.

## The reserved prefix

Channels with names starting with `_system/` are operator-yaml-only:

- Agents MAY subscribe to them (with ACL grant).
- Agents may NEVER publish to them — the Channel tool refuses
  on the prefix regardless of `publisher:` setting.
- Only loomcycle's internal Go publisher AND the admin endpoint
  may write.

This is the defense-in-depth gate: even if an operator forgets
to set `publisher: system` on a `_system/*` channel, agent
publishes still refuse.

## Three categories of system channels

**Cadence channels** publish on a fixed interval:

```yaml
_system/heartbeat-1m: { scope: global, publisher: system, period: 1m }
```

Loomcycle's heartbeat goroutine emits `{ts, version, uptime_s}` once
per period. Useful for "is the runtime alive" dashboards.

**Event-driven channels** publish on internal state transitions:

```yaml
_system/runtime-state:   { scope: global, publisher: system }
_system/provider-events: { scope: global, publisher: system }
```

No `period:` needed — these fire from internal subsystem hooks
(pause/resume, provider fallback, cache invalidation).

**Agent-publishable system channels** (no `publisher: system`):

```yaml
_system/alarms/critical: { scope: global, max_messages: 1000 }
```

Here `_system/` is reserved-by-convention but agents may publish
via the admin endpoint (or, in v0.8.8+, via a dedicated alarm tool).
Agent calls to `Channel.publish` still refuse the prefix.

## The admin endpoint

```
POST /v1/_channels/_system/{name...}
Authorization: Bearer <LOOMCYCLE_AUTH_TOKEN>
Content-Type: application/json

{ "payload": {...}, "deliver_at": "2026-...Z" }
```

Bearer-authed. Use cases:

- External monitoring → push alerts via webhook.
- Ops dashboards → operator-issued alarms.
- Operators debugging from `curl`.

The endpoint stamps `published_by_user_id = "_admin"` on the row
so audit queries can distinguish admin publishes from internal
ones (which use `"_system"`).

## Deferred publish (any channel)

`Channel.publish` accepts an optional `deliver_at` RFC3339 timestamp:

```
{"op":"publish", "channel":"findings",
 "value":{...},
 "deliver_at":"2026-05-12T15:30:00Z"}
```

The message lands in storage immediately but is hidden from
subscribers until `visible_at`. An in-process scheduler arms a
timer that wakes long-poll subscribers exactly at delivery time;
if the scheduler is over its cap or the process restarted,
subscribers see deferred messages on their next periodic poll.

TTL counts from `published_at`, NOT from `deliver_at`. A 1-hour
deferral with a 30-minute TTL means the message expires before
it becomes visible — effectively a no-op publish. Size your TTL
to cover both the deferral window and the visibility window.

Use cases for deferred publishes:

- Cooldown timers ("don't alarm again for 5 minutes").
- Scheduled handoffs ("kick off the batch job at 02:00").
- Retry-after-N patterns ("re-process this in 10 minutes").

## Discovering accessible system channels

Your `Context.channels` op (when granted in `subscribe:`) lists
which `_system/*` channels you can read.

```
{"op":"channels", "prefix":"_system/"}
→ {channels: [...], publish_wildcards: [...], subscribe_wildcards: [...]}
```

The `publisher` field on each entry tells you whether agents can
publish (`publisher: ""` = yes if your ACL grants it; `publisher: "system"`
= no).
