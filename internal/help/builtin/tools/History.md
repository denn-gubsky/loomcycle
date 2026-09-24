---
name: History
description: "History tool — browse, search, read and annotate PAST chats (a chat = one conversation session). Always pass scope: self is this agent's chats with every user, user is your own."
---
The `History` tool reaches **previous chats**. A chat is one conversation
session: it can span several runs, and it keeps its full transcript plus token,
cost and run-count totals. You can list and search chats, read one back, give
it a title, description or tags, pin or archive it, refresh its summary, get
the coordinates to continue it, find similar chats, and pull back the few
turns a remembered fact came from.

## When to use it — and when not

- **Use History** to find what was discussed in an EARLIER conversation, to
  read that conversation back, or to organise the chat list.
- **Do not use it for the chat you are in.** A detail from earlier in THIS
  conversation that has since been summarised away is `Recall`.
- **Do not use it as a memory store.** Titles, tags and pins are labels on a
  conversation, not facts. A fact you want to keep is `Memory op=set` or
  `Memory op=add`; a remembered fact is found with `Memory op=recall`. When a
  recalled fact is too condensed to answer from, `History op=window` fetches
  the turns it was distilled from.
- **Do not use it to inspect one run.** A chat groups runs; a run's own status
  and output are a run-level view.
- History never starts a run. `resume` only tells you how to continue a chat.

## Operations

Every operation takes `op`. Fetch one operation's article, with examples, as
`History/<op>` — for example `{"op":"help","topic":"History/search"}`.

| op | What it does | Required besides `op` |
|---|---|---|
| `list` | chats in a scope, pinned first then most recent, filtered and paged | — |
| `search` | chats whose title — or, with `match:"content"`, whose turns — match a query | `query` |
| `get` | one chat's metadata and its transcript | `session_id` |
| `window` | the few turns around the one a stored fact came from | `session_id`, `quote` |
| `rename` | set the chat's title | `session_id`, `title` |
| `annotate` | set its description and/or replace its tags | `session_id`, plus `description` or `tags` |
| `pin` | float it to the top of `list`, or unpin it | `session_id` |
| `archive` | hide it from `list` and `search`, reversibly | `session_id` |
| `recap` | have a model write a fresh short summary and store it on the chat | `session_id` |
| `resume` | the coordinates for continuing the chat in a new run | `session_id` |
| `related` | chats similar in meaning to a given chat or to free text | `session_id` or `query` |

## Scopes — always pass `scope`

`scope` chooses WHOSE chats you see. The owner comes from your run's identity,
never from the call, so you cannot reach someone else's chats by naming them.

- `user` — your own chats: this end-user's, with any agent, in this tenant.
- `self` — **this agent's chats with EVERY user**. Wider than `user`, not "you".
- `tenant` — every chat in this tenant.
- `global` — every tenant. Admin only.

**Omitting `scope` means `self`, but the grant an agent gets by default is
`user`.** So a call with no `scope` is usually refused with `history: scope
"self" not permitted (allowed: user)`. Pass `scope: "user"` unless you were
granted another and really want it. Every operation checks the scope,
including `get`, `rename` and `resume`. `user` also needs a user id on the run.

A chat outside the scope you name is reported as `history: chat "<id>" not
found`, exactly like a chat that does not exist. If you are sure the id is
right, the chat lives in a different scope. For how scopes work across every
tool, read `{"op":"help","topic":"scopes"}`.

## Things to know

- A chat id is the `session_id` that `list`, `search` and `related` return;
  Memory's `recall` returns it on a fact as `source_session_id`.
- `list`, `search` and `related` hide archived chats and chats served by the
  runtime's own maintenance agents; `include_archived` and `include_internal`
  bring them back. `get` by id reads either kind.
- Transcripts are stored with secrets already redacted.
- `recap` makes a model call and costs tokens. `related` and
  `search` with `match:"content"` need an embedder, and fail cleanly without one.
