---
name: History/annotate
description: "History op=annotate — set a chat's description and/or REPLACE its tag set."
---
`annotate` labels a chat with a description, tags, or both. Tags are what
`list` and `search` filter on with `tag`. **`tags` replaces the whole set** —
to add one tag, read the chat's current `tags` first and send them all back
with the new one.

## Arguments

- `session_id` (required) — the chat.
- `description` — the new description. An empty string clears it.
- `tags` — the new tag set, an array of strings. `[]` removes every tag.
- At least one of `description` and `tags` is required; a field you leave out
  is left unchanged.
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

## Returns

`{scope, chat}` — the chat's updated metadata, including `description` and
`tags` (see `History/rename` for the full field list).

## Errors

- `history: annotate requires description and/or tags` — pass at least one.
- `history: chat "<id>" not found` — no such chat in THIS scope. Check the
  scope; the same call will fail the same way.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Describe a chat and tag it:

```json
{"op": "annotate", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "description": "Agreed the new launch date and who covers for Maria.", "tags": ["launch", "planning"]}
```

```json result
{"scope": "user", "chat": {"session_id": "b4407b522dc11495d3de371311db17f0",
 "description": "Agreed the new launch date and who covers for Maria.", "tags": ["launch", "planning"], "run_count": 2}}
```

Add a tag to a chat already tagged `launch` and `planning` — send the whole set:

```json
{"op": "annotate", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "tags": ["launch", "planning", "decided"]}
```
