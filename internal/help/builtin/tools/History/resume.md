---
name: History/resume
description: "History op=resume — the coordinates for continuing a chat in a new run. It does NOT start a run and returns no transcript."
---
`resume` hands back what you need to continue a chat: its id, the agent that
served it, its owner, its status and last activity, and a hint. **It does not
start a run**, and it does not return the transcript — use `History/get` to
read the chat. A chat is continued by starting a new run against its
`session_id` (over the HTTP API or the `spawn_run` MCP tool); that run appends
to the same transcript.

## Arguments

- `session_id` (required) — the chat.
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

## Returns

`{scope, resume: {session_id, agent, tenant_id, user_id, status,
last_activity, hint}}`. `status` is `running` while any run of the chat is
still going, otherwise the most recent run's status. `hint` spells out the
request that continues the chat.

## Errors

- `history: resume requires session_id`.
- `history: chat "<id>" not found` — no such chat in THIS scope. Check the
  scope; the same call will fail the same way.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Get the continuation handle for one of your chats:

```json
{"op": "resume", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0"}
```

```json result
{"scope": "user", "resume": {"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat",
 "user_id": "u-4821", "status": "completed", "last_activity": "2026-09-22T14:03:11Z",
 "hint": "Continue this chat by starting a new run against this session_id — POST /v1/runs with {\"session_id\":\"b4407b522dc11495d3de371311db17f0\",\"agent\":\"chat\", ...} (or the spawn_run MCP tool). The new run appends to this chat's transcript."}}
```
