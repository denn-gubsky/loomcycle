---
name: History/get
description: "History op=get — one chat's metadata and its WHOLE transcript, as an event array, a Markdown export, or just the user/assistant turns."
---
`get` reads one chat back: its labels and totals, plus its **whole**
transcript. To read a past conversation yourself, pass `format:
"conversation"` — you get only what the user and the assistant said, as
Markdown, without tool traffic or runtime events. A long chat is long in every
format; when you only need the part a remembered fact came from, use
`History/window` instead.

## Arguments

- `session_id` (required) — the chat, as returned by `list` or `search`.
- `scope` (pass it) — `user`, `self`, `tenant` or `global`; the chat must be
  in this scope. Omitted means `self`, which is usually not granted.
- `format` — how to return the transcript:
  - omitted — a structured event array;
  - `"conversation"` — only the user and assistant turns, as Markdown;
  - `"markdown"` — a human export: a metadata header, the summary, then every
    event, with events that have no text shown as JSON.
  Any other value is treated as omitted.

## Returns

Always `scope` and `chat`. `chat` has `session_id`, `tenant_id`, `agent`,
`user_id`, `created_at`, `run_count`, `input_tokens`, `output_tokens`, `cost`,
and when set `title`, `description`, `tags`, `pinned`, `archived`, `summary`.

- No `format`: `transcript: [{seq, run_id, ts, type, payload}]`, oldest first.
  `type` is the event kind, such as `user_input`, `text`, `tool_call`,
  `tool_result`; assistant text arrives as many small `text` events.
- `format: "conversation"` or `"markdown"`: `format` echoes the value and
  `markdown` holds the rendered text. In the conversation form each turn is a
  `### user` or `### assistant` heading followed by the words.

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

The full structured event log of the same chat:

```json
{"op": "get", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0"}
```
