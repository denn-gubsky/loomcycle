---
name: History/list
description: "History op=list — the chats in one scope, pinned first then most recent, with filters and offset paging."
---
`list` returns the chats in one scope, **pinned first, then most recent**, a
page at a time. Each row carries the chat's labels and its token, cost and
run-count totals, but no transcript — `get` one chat to read it. Pass `scope`:
leaving it out means `self`, which is usually not granted.

## Arguments

- `scope` — `user` (your own chats), `self` (this agent's chats with every
  user), `tenant` or `global`. Omitted: `user` when granted, else `self`.
- `status` — only chats whose derived status is `running`, `completed`,
  `failed` or `cancelled`. A chat is `running` while any of its runs is.
- `from`, `to` — RFC3339 bounds on the chat's last activity, e.g.
  `"2026-09-01T00:00:00Z"`.
- `tag` — only chats carrying this exact tag.
- `title_contains` — case-insensitive substring of the title.
- `pinned_only` — `true` returns only pinned chats.
- `include_archived` — `true` includes archived chats (hidden by default).
- `include_internal` — `true` includes chats served by the runtime's own
  maintenance agents (hidden by default).
- `limit` — chats per page (default 50, at most 500).
- `offset` — rows to skip, for the next page.

## Returns

`{scope, chats: [...], total, limit, offset}`. Each chat has `session_id`,
`tenant_id`, `agent`, `user_id`, `created_at`, `last_activity`, `run_count`,
`input_tokens`, `output_tokens`, `cost`, `status`, and when set `title`,
`description`, `tags`, `pinned`, `archived`, `summary`. `total` counts every
matching chat, not just this page; `limit` is the page size actually applied.

## Errors

- `history: scope "self" not permitted (allowed: user)` — you left out `scope`
  or named one you are not granted. Retry with a scope from the allowed list.
- `history: user scope needs a user identity in the run context` — this run
  has no user. Use a scope you hold that does not need one, such as `tenant`
  if granted; retrying `user` is pointless.
- `history: unknown status "..."` — use one of the four statuses above.
- `history: from must be RFC3339` / `to must be RFC3339` — send a full
  timestamp with a zone, like `2026-09-01T00:00:00Z`.
- An empty `chats` list is not an error: nothing in THIS scope matched.

## Examples

Your own most recent chats, 20 at a time:

```json
{"op": "list", "scope": "user", "limit": 20}
```

```json result
{"scope": "user", "total": 57, "limit": 20, "offset": 0, "chats": [
  {"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat", "title": "Q3 budget review",
   "pinned": true, "run_count": 3, "input_tokens": 18230, "output_tokens": 2411, "cost": 0.041,
   "status": "completed", "last_activity": "2026-09-22T14:03:11Z", "created_at": "2026-09-20T09:12:40Z"}]}
```

The next page of that listing:

```json
{"op": "list", "scope": "user", "limit": 20, "offset": 20}
```

Tagged chats from September, including archived ones:

```json
{"op": "list", "scope": "user", "tag": "billing", "from": "2026-09-01T00:00:00Z", "to": "2026-09-30T23:59:59Z", "include_archived": true}
```
