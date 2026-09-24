---
name: History/rename
description: "History op=rename — set a chat's title, so it is easy to find with list and search."
---
`rename` sets a chat's title. Titles are usually auto-generated and vague, and
the default `search` matches on the title, so a clear title is what makes a
chat findable later. Only the label changes; the transcript is untouched.

## Arguments

- `session_id` (required) — the chat to rename.
- `title` (required) — the new title. An empty string clears it.
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

## Returns

`{scope, chat}` — the chat's updated metadata: `session_id`, `agent`,
`title`, `created_at`, `run_count`, `input_tokens`, `output_tokens`, `cost`,
and any `description`, `tags`, `pinned`, `archived`, `summary`.

## Errors

- `history: rename requires title` — pass `title`.
- `history: chat "<id>" not found` — no such chat in THIS scope. Check the
  scope; the same call will fail the same way.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Give one of your chats a descriptive title:

```json
{"op": "rename", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "title": "Launch date moved to the 14th"}
```

```json result
{"scope": "user", "chat": {"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat",
 "title": "Launch date moved to the 14th", "run_count": 2, "input_tokens": 9120, "output_tokens": 1305, "cost": 0.018}}
```
