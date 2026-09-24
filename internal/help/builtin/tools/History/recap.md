---
name: History/recap
description: "History op=recap — have a model write a fresh one-or-two-sentence summary of a chat and store it on the chat. Costs a model call over the whole transcript."
---
`recap` sends a chat's transcript to a model, gets back a one-or-two-sentence
summary (about 256 characters at most), and stores it on the chat, so `list`,
`search` and `get` show it without anyone re-reading the transcript. It also
makes the chat findable by `related`. **It is a real model call, and its cost
grows with the length of the chat** — recap a chat when its summary is
missing or stale, not on every turn.

It is safe on a chat that is still running or waiting for input: it reads the
transcript so far and never touches the run. Running it again replaces the
previous summary. A chat that has been compacted is summarised from its
compacted form.

## Arguments

- `session_id` (required) — the chat to summarise.
- `scope` (pass it) — the scope the chat is in, usually `user`. Omitted means
  `self`, which is usually not granted.

The summary is written by the model of the agent that SERVED the chat (its
cheaper compaction model when it has one), not by your own model.

## Returns

`{scope, summary, chat}` — the new summary, and the chat's updated metadata
with it in `summary`.

## Errors

- `history: recap: chat has no transcript to recap yet` — nothing has been
  said in the chat. Retrying is pointless until it has.
- `history: recap: the chat's agent no longer exists` — the agent that served
  the chat has been removed, so there is no model to summarise with. Not
  retryable.
- `history: recap: resolve provider/model: ...` or `history: recap:
  summarize: ...` — the model call failed. One retry later is reasonable.
- `history: recap not configured (no summarizer wired)` — this deployment
  cannot recap. Not retryable.
- `history: chat "<id>" not found` — no such chat in THIS scope. Check the
  scope.

## Examples

Refresh the summary of one of your chats:

```json
{"op": "recap", "scope": "user", "session_id": "b4407b522dc11495d3de371311db17f0"}
```

```json result
{"scope": "user", "summary": "Moved the launch to the 14th because Maria is out the week of the 7th; Sam covers QA.",
 "chat": {"session_id": "b4407b522dc11495d3de371311db17f0", "agent": "chat", "title": "Planning sync",
          "summary": "Moved the launch to the 14th because Maria is out the week of the 7th; Sam covers QA.", "run_count": 2}}
```
