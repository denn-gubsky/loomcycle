---
name: History/search
description: "History op=search — find chats by title (default) or, with match:content, by what was actually said in your own conversations."
---
`search` finds chats by a query. By default it matches the chat's **title**,
which is usually auto-generated, so a title search often misses a chat you
remember well. When you remember WHAT was said but not what the chat was
called, pass `match: "content"`: it searches the turns of your own
conversations by meaning and returns each chat with the turn that matched.

## Arguments

- `query` (required) — the text to look for.
- `scope` — `user` (your own chats), `self` (this agent's chats with every
  user), `tenant` or `global`. Omitted means `self`, which is usually not
  granted — pass it.
- `match` — `title` (default) or `content`.
- `limit` — chats per page (default 50, at most 500).

With the default title match, the `list` filters also apply: `status`,
`from`, `to`, `tag`, `pinned_only`, `include_archived`, `include_internal`
and `offset` (see `History/list`). The query replaces `title_contains`.

With `match: "content"`, those filters and `offset` are ignored. Only turns
indexed under YOUR user id are searched, whatever the scope; the scope then
drops any chat outside it. At most 60 matching turns are considered, so a
content search returns a handful of chats, not a full page.

## Returns

Title match: the same shape as `list` — `{scope, chats, total, limit, offset}`.

Content match: `{scope, match: "content", chats, matched_turns, total, limit}`.
`matched_turns` lines up with `chats` by position; each is
`{session_id, text, speaker, score}`, the best-matching turn of that chat.
Chats come back best match first.

## Errors

- `history: search requires a non-empty query`.
- `history: scope "self" not permitted (allowed: user)` — pass `scope` with a
  value from the allowed list.
- `history: unknown match "..."` — use `title` or `content`.
- `history: match=content needs an embedder` — none is configured here. Fall
  back to the title match; retrying `content` is pointless.
- `history: match=content searches the turns YOU typed, and this run carries no
  user_id` — use the title match instead.
- No chats is not an error. A title search that finds nothing is worth one
  retry with `match: "content"`.

## Examples

Your own chats with "invoice" in the title:

```json
{"op": "search", "scope": "user", "query": "invoice"}
```

The chat where the user talked about moving the launch date, found by what
was said rather than by its title:

```json
{"op": "search", "scope": "user", "query": "moving the launch to a later date", "match": "content", "limit": 5}
```

```json result
{"scope": "user", "match": "content", "total": 1, "limit": 5,
 "chats": [{"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat", "title": "Planning sync",
            "run_count": 2, "input_tokens": 9120, "output_tokens": 1305, "cost": 0.018}],
 "matched_turns": [{"session_id": "b4407b522dc11495d3de371311db17f0", "speaker": "user",
                    "text": "Let's push the launch to the 14th, Maria is out that week.", "score": 0.83}]}
```
