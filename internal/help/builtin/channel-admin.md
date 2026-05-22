---
name: channel-admin
description: External HTTP/gRPC/MCP/TS surface for publish/subscribe/peek/ack on operator-declared channels.
---
The v0.8.4 Channel tool gives agents inside loomcycle a pub/sub
primitive over operator-declared channels. The v0.9.x **Channel CRUD**
surface extends the same four ops (publish / subscribe / peek / ack)
to external callers via HTTP, gRPC, the LoomCycle MCP server, and
the TS adapter. Bearer-authed; same store + bus + cursor semantics as
the in-band tool.

Use this when an external orchestrator needs to publish into the
loomcycle pub/sub fabric without going through a one-shot agent. The
load-bearing example is the **n8n-nodes-loomcycle** community node:
n8n's Slack/HubSpot/cron triggers publish to a loomcycle channel; a
loomcycle agent subscribed to that channel processes the message.
Before v0.9.x this required a "one-shot agent that just publishes"
detour; the new surface lets the n8n node hit the Channel directly.

## What this is NOT

Read this section before anything else.

- **NOT a replacement for the in-band Channel tool.** Agents inside
  loomcycle still use the tool — same ops, same wire shape, lives in
  agent context. The CRUD surface is for OUTSIDE callers.
- **NOT a different channel namespace.** Publishes from external
  callers land on the SAME table rows as publishes from agents.
  Subscribers (agent or external) wake on the same `Bus.Notify`.
- **NOT a per-agent escalation path.** No agent's `allowed_tools`
  includes this surface. It's reachable only via the operator's
  bearer token over HTTP / gRPC / MCP / TS adapter.
- **NOT system-channel publishing.** Channels under `_system/*` keep
  their existing admin-only route at `POST /v1/_channels/{name...}`.
  The new sub-paths gate the wider channel set.
- **NOT streaming.** Subscribe is a single-round-trip long-poll — one
  request, one response (whatever's there now plus an optional wait
  for new publishes). Callers wanting continuous delivery loop
  subscribe themselves. SSE was deliberate-not-built; long-poll
  composes more cleanly with n8n's worker model.

## Two URL families

Both gate on the operator's `LOOMCYCLE_AUTH_TOKEN` and call the same
underlying store + Bus helpers. The split is about scope:

**Admin (scope=global)** — operator-level addressing, cursor namespace
keyed by channel name only:

```
POST /v1/_channels/{name}/publish
POST /v1/_channels/{name}/subscribe
GET  /v1/_channels/{name}/peek
POST /v1/_channels/{name}/ack
```

**Per-user (scope=user)** — `scope_id` derived from the URL path so
callers can't forge a different `user_id` by lying in the body:

```
POST /v1/users/{user_id}/channels/{name}/publish
POST /v1/users/{user_id}/channels/{name}/subscribe
GET  /v1/users/{user_id}/channels/{name}/peek
POST /v1/users/{user_id}/channels/{name}/ack
```

`{name}` is a single URL segment. Channel names containing slashes
(e.g. `findings/alpha`) URL-encode the slash (`findings%2Falpha`).

The existing system-publish route at `POST /v1/_channels/{name...}`
keeps its semantics — Go 1.22+ ServeMux picks the more-specific
pattern when both could match a URL, so the new sub-paths don't
collide.

## Op semantics

Mirror of the in-band tool. The wire docs cover the deltas:

- **publish** — write a message. Optional `deliver_at` (RFC3339Nano)
  defers delivery so long-poll subscribers wake at `visible_at`.
  Returns `{msg_id, channel, created_at, visible_at?}`. The
  `_admin` audit attribution is used for scope=global; the user_id
  for scope=user.
- **subscribe** — single-round-trip long-poll. Returns immediately
  if messages are present; otherwise waits up to `wait_ms` (capped
  at the operator's `ChannelsLongPollCapMS`, default 30s) for a
  publish. **Auto-commits the cursor on a non-empty batch** —
  at-most-once shape, same as the in-band tool's subscribe op.
  Returns `{channel, messages, next_cursor}`.
- **peek** — non-destructive read. Defaults to `cur_0` (replay from
  oldest) when `from_cursor` is empty so a fresh-bearer caller can
  read history without disturbing any subscriber's progress.
  Returns `{channel, messages}` (no `next_cursor`).
- **ack** — explicit cursor advance for at-least-once consumption.
  Cursor must be monotonically forward — older cursors return HTTP
  409 with `code: channel_cursor_regression` (the typed
  `ChannelCursorRegressionError` on the TS adapter).

## Operator workflow

Same shape across HTTP / gRPC / MCP / TS adapter. The HTTP routes are
the simplest illustration:

```bash
# Publish — admin surface
curl -X POST http://127.0.0.1:8787/v1/_channels/team-updates/publish \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"payload": {"event": "deploy-completed", "service": "api"}}'

# Subscribe with 5-second long-poll
curl -X POST http://127.0.0.1:8787/v1/_channels/team-updates/subscribe \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"wait_ms": 5000, "max_messages": 25}'

# Peek (operator-side debug — does NOT consume)
curl "http://127.0.0.1:8787/v1/_channels/team-updates/peek?max_messages=5" \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN"

# Ack a specific cursor (at-least-once pattern)
curl -X POST http://127.0.0.1:8787/v1/_channels/team-updates/ack \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"cursor": "cur_..."}'

# Per-user variant — same body, scope_id = "alice" from the URL
curl -X POST http://127.0.0.1:8787/v1/users/alice/channels/inbox/publish \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"payload": {"subject": "new lead match"}}'
```

The TS adapter (`@loomcycle/client@^0.13`) wraps this in four typed
methods:

```ts
import { LoomcycleClient } from "@loomcycle/client";

const c = new LoomcycleClient({ baseUrl, bearerToken });

await c.publishChannel("team-updates", {
  scope: "global",
  payload: { event: "deploy-completed" },
});

const batch = await c.subscribeChannel("team-updates", {
  scope: "global",
  waitMs: 5000,
});
// batch.messages: [{id, value, published_at}, ...]
// batch.next_cursor: "cur_..."
```

## Error responses

| HTTP | Wire `code` | When | Adapter error class |
|---|---|---|---|
| 400 | `channel_scope_invalid` | scope is not "global" or "user" | `InvalidArgumentError` |
| 400 | `invalid_user_id` | user_id doesn't match `[A-Za-z0-9_-]{1,128}` | `InvalidArgumentError` |
| 401 | `unauthorized` | bearer mismatch | `AuthError` |
| 404 | `channel_not_declared` | channel not in operator yaml | `NotFoundError` |
| 409 | `channel_cursor_regression` | ack cursor older than committed | `ChannelCursorRegressionError` |
| 503 | `system_publisher_unwired` | server constructed without SetSystemPublisher | `UnavailableError` |

gRPC errors map to the corresponding `codes.*` (NotFound,
InvalidArgument, FailedPrecondition, Unavailable). MCP meta-tools
return `{isError: true, content: [{text: ...}]}` with the same
underlying messages.

## When to use each URL family

- **Admin (scope=global)**: operator-level pub/sub, alerts the whole
  org should see, broadcast notifications, audit-log streams.
- **Per-user (scope=user)**: per-end-user inboxes, per-end-user
  notifications, n8n workflows acting on behalf of a specific user.

The same channel name CAN be declared once and used at both scopes
(the cursor namespace is `(channel, scope, scope_id)`-keyed) — but
mixing scopes on the same name is operator-confusing. Pattern: pick
one scope per declared name.

## References

- `help(topic="loomcycle")` — the runtime + tool model the Channel
  surface lives inside.
- `help(topic="scopes")` — agent vs user vs global scope semantics
  shared across Memory + Channel.
- `help(topic="n8n-integration")` — the three composition patterns
  (n8n's AI Agent → loomcycle MCP; loomcycle → n8n MCP Server
  Trigger; planned community node). Pattern 1 + 3 both rely on this
  Channel CRUD surface for the loomcycle-side trigger plumbing.
- `internal/tools/builtin/channel.go` — the in-band tool agents use.
  Same ops, same store/bus; different boundary.
