---
name: History/archive
description: "History op=archive — hide a chat from list, search and related, reversibly; archived:false restores it. Nothing is deleted."
---
`archive` hides a chat from `list`, `search` and `related`. It is a soft,
reversible hide: **nothing is deleted**, `get` by id still reads the chat, and
`archived: false` brings it back. It archives by default.

## Arguments

- `session_id` (required) — the chat.
- `archived` — `true` archives (default), `false` restores.
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

## Returns

`{scope, chat}` — the chat's updated metadata; `archived: true` appears only
while it is archived (see `History/rename` for the full field list).

To see archived chats afterwards, pass `include_archived: true` to `list` or
`search`.

## Errors

- `history: archive requires session_id`.
- `history: chat "<id>" not found` — no such chat in THIS scope. Check the
  scope; the same call will fail the same way.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Archive a finished chat:

```json
{"op": "archive", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0"}
```

Restore it:

```json
{"op": "archive", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "archived": false}
```
