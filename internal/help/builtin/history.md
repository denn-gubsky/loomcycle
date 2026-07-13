---
name: history
description: History — browse, search, and annotate PAST chats (a chat = a conversation session). Owner-scoped (self/user/tenant/global), tenant-isolated, with list/get/search/rename/annotate/pin/archive/recap/resume.
---
The `History` tool lets an agent — or an operator over HTTP/MCP — reach **previous
chats**: list them, search by title, read a full transcript, and give a chat a
human **title / description / tags**, or **pin** / **archive** it. A "chat" is a
conversation **session** (session → runs → events); a chat may span several runs.

It replaces the old `Context op=history` (which had no listing, no search, no
annotation, and a flat cross-tenant `any`). Every read is tenant-isolated.

## Scopes — whose chats you see

`scope` selects the owner; the owner *id* is always resolved server-side from
your run identity, **never** from the request — so you can't ask for someone
else's chats by passing their id.

- `self` (default) — this agent's chats (sessions whose agent is you).
- `user` — this end-user's chats (your `user_id`, within your tenant).
- `tenant` — every chat in your tenant.
- `global` — chats across **all** tenants. Cross-tenant, so it is **admin-only**:
  a tenant-scoped run has `global` stripped even if the agent's yaml lists it.

Access is gated by the agent's `history_scope` (default-deny — an agent with no
`history_scope` can't use History at all). Grant it in yaml, e.g.
`history_scope: [self, user]`. A by-id op (`get`/`rename`/…) on a chat outside
your scope returns an opaque **not found** — the fold never reveals that a chat
exists in another tenant.

## Ops

- `list [scope] [status] [from] [to] [tag] [title_contains] [pinned_only] [include_archived] [limit] [offset]`
  — chats in the scope, **pinned first then most-recent**, paginated. Each row
  carries the chat's metadata plus **token / cost / run-count** aggregates.
  Archived chats are excluded unless `include_archived:true`.
- `get session_id [format]` — one chat: its metadata + the full transcript.
  `format:"markdown"` renders the transcript as a Markdown document (a baked-in
  export) instead of a structured event array.
- `search query [scope] [+ the list filters]` — case-insensitive **title** match
  within a scope. (Metadata MVP; full-text content search over event bodies is
  not yet available.)
- `rename session_id title` — set the chat's title.
- `annotate session_id [description] [tags]` — set the description and/or replace
  the tag set.
- `pin session_id [pinned]` — float the chat to the top (`pinned:false` unpins).
- `archive session_id [archived]` — reversible soft-hide (`archived:false`
  restores). Distinct from the usage-retention pruner, which hard-deletes.
- `recap session_id` — refresh the chat's **stored LLM summary** of the
  transcript-so-far. Idempotent (re-running replaces it) and safe on a **live /
  parked** chat, so an in-loop agent or an operator can keep a fresh "what this
  chat is about" — e.g. call it periodically while an interactive run waits for
  input. The summary is written to the chat's metadata, so `list` / `get` /
  `search` then surface it cheaply without re-reading the transcript. Uses the
  agent's compaction model when configured, else its resolved tier model.
- `resume session_id` — return a **continuation handle** for the chat:
  `{session_id, agent, tenant_id, user_id, status, last_activity, hint}`. It does
  not itself start a run — the `hint` explains how to continue (a new run against
  this `session_id`).

## Notes

- **Redaction:** transcripts are stored already-scrubbed (secrets in tool
  inputs / results are redacted at write time), so `get` never surfaces a secret
  that redaction would have masked.
- Continuing a past chat is a `POST /v1/runs` with its `session_id` (what
  `resume` hands you the coordinates for) — History reads, annotates, and recaps
  chats; it does not itself start runs.
- `recap` summarizes the chat's **current effective** transcript, so a chat that
  has already been context-compacted is recapped from its post-compaction view.
