---
name: History/pin
description: "History op=pin — float a chat to the top of list, or unpin it with pinned:false."
---
`pin` marks a chat as pinned, so `list` returns it before every unpinned chat
and `pinned_only` can select it. It pins by default; pass `pinned: false` to
unpin. Nothing else about the chat changes.

## Arguments

- `session_id` (required) — the chat.
- `pinned` — `true` pins (default), `false` unpins.
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

## Returns

`{scope, chat}` — the chat's updated metadata; `pinned: true` appears only
while it is pinned (see `History/rename` for the full field list).

## Errors

- `history: pin requires session_id`.
- `history: chat "<id>" not found` — no such chat in THIS scope. Check the
  scope; the same call will fail the same way.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Pin one of your chats:

```json
{"op": "pin", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0"}
```

Unpin it again:

```json
{"op": "pin", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "pinned": false}
```
