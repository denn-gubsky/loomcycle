---
name: History/get
description: "History op=get — one chat's metadata and its transcript, paged by conversation turn, as an event array, a Markdown export, or just the user/assistant turns."
---
`get` reads one chat back: its labels and totals, plus its transcript. To
read a past conversation yourself, pass `format: "conversation"` — you get only
what the user and the assistant said, as Markdown, without tool traffic or
runtime events.

A long chat is read in **pages of conversation turns**. Inside a run a page is
capped to fit your context: when the result says `has_more: true`, call `get`
again with `offset` set to its `next_offset`. Use `limit` to take fewer turns,
and `from` / `to` to read only the turns said in a time range. When you only
need the part a remembered fact came from, use `History/window` instead.

## Arguments

- `session_id` (required) — the chat, as returned by `list` or `search`.
- `scope` (pass it) — `user`, `self`, `tenant` or `global`; the chat must be
  in this scope. Omitted: `user` when granted, else `self`.
- `format` — how to return the transcript:
  - omitted — a structured event array;
  - `"conversation"` — only the user and assistant turns, as Markdown;
  - `"markdown"` — a human export: a metadata header, the summary, then every
    event, with events that have no text shown as JSON.
  Any other value is treated as omitted.
- `offset` — the turn to start at, counting from 0. Pass the previous page's
  `next_offset` to read on.
- `limit` — at most this many turns. Omitted: all that fit (inside a run, a
  page is capped to fit your context).
- `from` / `to` — RFC3339 times; keep only the turns said inside the range.
  `offset` then counts within it.

## Returns

Always `scope`, `chat`, and where the page sits: `turns_total` (turns in the
selected range), `offset`, `turns_returned`, `has_more`, and `next_offset` when
there is more. `chat` has `session_id`, `tenant_id`, `agent`,
`user_id`, `created_at`, `run_count`, `input_tokens`, `output_tokens`, `cost`,
and when set `title`, `description`, `tags`, `pinned`, `archived`, `summary`.

- No `format`: `transcript: [{seq, run_id, ts, type, payload}]`, oldest first.
  `type` is the event kind, such as `user_input`, `text`, `tool_call`,
  `tool_result`; assistant text arrives as many small `text` events.
- `format: "conversation"` or `"markdown"`: `format` echoes the value and
  `markdown` holds the rendered text. In the conversation form each turn is a
  `### user` or `### assistant` heading followed by the words.
- `truncated: true` and a `note` when one turn alone was larger than a page
  and had to be cut.

## Errors

- `history: chat "<id>" not found` — the id does not exist OR the chat is in
  another scope. Check the scope before retrying; the same call will fail the
  same way.
- `history: get requires session_id`.
- `history: scope "self" not permitted (allowed: user)` — pass `scope`.

## Examples

Read a past conversation of your own, turns only:

```json
{"op": "get", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "format": "conversation"}
```

```json result
{"scope": "user", "format": "conversation",
 "chat": {"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat", "title": "Planning sync", "run_count": 2},
 "markdown": "### user\n\nLet's push the launch to the 14th.\n\n### assistant\n\nDone — I moved the launch to the 14th.\n\n"}
```

A long chat, read in pages — the first page:

```json
{"op": "get", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "format": "conversation"}
```

```json result
{"scope": "user", "format": "conversation",
 "chat": {"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat", "run_count": 9},
 "turns_total": 64, "offset": 0, "turns_returned": 18, "has_more": true, "next_offset": 18,
 "markdown": "### user\n\n…"}
```

…and the next one:

```json
{"op": "get", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "format": "conversation", "offset": 18}
```

Only what was said on one afternoon:

```json
{"op": "get", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0", "format": "conversation", "from": "2026-09-24T13:00:00Z", "to": "2026-09-24T18:00:00Z"}
```

The full structured event log of the same chat:

```json
{"op": "get", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0"}
```
