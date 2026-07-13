# History (RFC BE)

History is the built-in tool for reaching **past chats** — listing them,
searching, reading a transcript, and giving a chat a human title / description /
tags, or pinning / archiving it. A "chat" is a conversation **session** (session
→ runs → events); a chat may span several runs, and its metadata lives on the
`sessions` row (added in PR 1).

**Why a dedicated tool, not "just keep using `Context op=history`":** the old op
was agent-relationship scoped (`self` / `any`), returned a single session's raw
events by `agent_id`, had **no** listing, **no** search, **no** annotation, and
its `any` scope read across tenants flat (no isolation). Chats had no
human handle at all. History replaces it with proper **owner-scope axes**, a
tenant-safe visibility fold on every read, and metadata you can set and browse.
`Context op=history` is removed — History is the sole history surface (no
overlapping, drifting pair of tools).

## Scope model — the load-bearing part

`scope` selects **whose** chats you reach. The owner *id* is always resolved
**server-side** from the run identity (`RunIdentity(ctx)` / `AgentName(ctx)`),
never from the request body — the same non-negotiable rule the Memory tool
follows for `scope_id`. A model-supplied owner id would let one tenant's agent
read another's chats, so the wire only ever carries the scope *selector*.

| scope    | whose chats | filter built |
|----------|-------------|--------------|
| `self` (default) | this agent's | `agent == AgentName(ctx)` + caller's tenant |
| `user`   | this end-user's | caller's `user_id` + caller's tenant |
| `tenant` | this tenant's | caller's tenant |
| `global` | **all tenants** | no tenant filter — **admin only** |

Access is gated by the per-agent **`history_scope`** yaml (default-deny — an
agent with no `history_scope` cannot use History). It is the same field the old
op used, repurposed to this owner-scope vocabulary:

```yaml
agents:
  support-triage:
    tools: [History]
    history_scope: [self, user]      # its own + the end-user's chats
```

Legacy migration: the old `self` is kept; `any` is accepted as an **alias for
`global`**; the never-implemented `siblings` / `descendants` / `named:<n>` values
are retired (config-load rejects them with a message pointing at the new set).

### Admin-gated `global`

`global` is cross-tenant, so it is honored **only under an admin principal**. The
gate lives at policy-resolution time, not in the tool:
`server.go historyPolicyForAgent` (in-loop) and `grantOperatorPolicies` (MCP) /
`substrateAdminCtx` (off-run HTTP) **strip `global`** from the resolved scope
list for a non-admin principal — so a tenant agent whose yaml lists `global`
still resolves only its own tenant. The tool then simply trusts `policy.Scopes`;
no separate admin check lives inside the tool.

The admin gate **fails closed**: an absent principal (open dev mode, or a resumed
run re-dispatched at boot with no request context) is treated as non-admin and
loses `global`. That loses nothing operationally — with no tenant boundary,
`scope:tenant` already spans everything — while guaranteeing a resumed tenant run
can never silently regain cross-tenant reach.

### Tenant fold on by-id reads

`get` / `rename` / `annotate` / `pin` / `archive` take a `session_id`. Before
touching the row, the tool fetches it and enforces the resolved scope's owner
constraint (tenant match for `tenant`/`self`; tenant+user for `user`;
tenant+agent for `self`; any for admin `global`). A row outside the scope — and a
row that doesn't exist — return the **same opaque "not found"**: the fold never
becomes a cross-tenant existence oracle (session ids are not secret).

## Ops

| op        | what it does |
|-----------|--------------|
| `list`    | chats in the scope, **pinned-first then most-recent**, paginated. Filters: `status`, `from`/`to` (RFC3339, on last-activity), `tag`, `title_contains`, `pinned_only`, `include_archived`, `limit`, `offset`. Each row carries token/cost/run-count aggregates. |
| `get`     | one chat: metadata + the full transcript. `format:"markdown"` renders the transcript as a Markdown export instead of a structured event array. |
| `search`  | case-insensitive **title** match within the scope (`query`). Metadata MVP — full-text content search over event bodies is deferred (an FTS index, additive later). |
| `rename`  | set the chat's `title`. |
| `annotate`| set `description` and/or replace `tags`. |
| `pin`     | float the chat to the top (`pinned:false` unpins). |
| `archive` | reversible soft-hide (`archived:false` restores). Distinct from the RFC AV usage-retention pruner, which hard-deletes. |
| `recap`   | refresh the chat's **stored LLM summary** of the transcript-so-far — idempotent, and safe on a **live / parked** chat. Written to the chat's metadata so `list`/`get`/`search` surface it cheaply. See below. |
| `resume`  | return a **continuation handle** `{session_id, agent, tenant_id, user_id, status, last_activity, hint}` — the coordinates + hint for continuing the chat in a new run. Does not itself start a run. |
| `related` | find chats **similar in meaning** to a given chat (`session_id`, excluded from its own results) or a free-text `query`, using the configured embedder. Ranked by cosine similarity, folded to the same scope. **Gated on an embedder.** See below. |

## Recap — a metadata-backed, refreshable summary

`recap` produces an LLM summary of a chat's transcript-so-far and **stores it** on
the chat's metadata (`sessions.summary`, stamping `summary_updated_at`). It is:

- **Idempotent** — re-running replaces the cached summary; the metadata always
  holds the latest "what this chat is about".
- **Live-safe** — it reads the transcript and never touches the run loop, so an
  in-loop agent (or an operator) can recap a chat that is still running or parked
  `awaiting_input`. Designed to be called periodically during long user-input
  waits so `list` / `get` / `search` surface a fresh summary without re-reading
  the whole transcript.
- **Fold-first** — the scope fold runs before any summarization, so a cross-scope
  recap is refused with the same opaque not-found as any other by-id op.

The tool itself never calls a provider — the server injects the summarizer
(`Server.RecapSession`), the off-loop, session-scoped twin of the context-
compaction summary step (`loop.Summarize`). It replays the **whole** session
transcript (a chat spans every run, like resume), resolves provider + model the
same way compaction does — the agent's `compaction.model` when set, else its
resolved tier model — and carries the chat's RFC AX operator-key restriction
(from its most recent run) so a restricted tenant's recap never spends the
operator key. A recap of an already-compacted chat summarizes its **current
effective** (post-compaction) view.

## Related — semantic "similar chats"

`related` finds chats close in meaning to a source, using the **same vector
embedder the Memory tool uses** (`memory.embedder` in operator yaml). The source
is either a chat (`session_id` — its title + summary + description is embedded and
the chat is excluded from its own results) or a free-text `query`. It is **gated
on an embedder**: with none configured it refuses cleanly (`related requires an
embedder …`), mirroring Memory's `ErrEmbedderNotConfigured` posture; every other
History op works without one.

**The index and how it fills.** A dedicated `session_embeddings` table (one row
per chat, migration 0058) holds each chat's vector. It is populated **lazily,
best-effort**: `recap` (summary change), `rename` (title change), and `annotate`
(description change) re-embed the chat and upsert its row. A populate failure is
logged and never fails the triggering op. There is **no historical backfill** in
this PR — an old chat becomes matchable only after it is next recapped / renamed /
annotated. The `sessionEmbedText` helper (title + summary + description) is the
single source of embed text, so a chat's stored vector and a by-id `related`
lookup of it are computed identically.

**No pgvector needed.** Unlike `memory_embeddings` (which needs pgvector /
sqlite-vec), the per-chat index is small, so the vector is a plain TEXT column
(`store.EncodeVector`) and cosine ranking runs **in Go** over the folded
candidate set (`store.CosineSimilarity`). This keeps `related` working on the
default single-binary SQLite build — and lets the same real search run in CI on
both backends. A recency cap (`sessionSimilarCandidateCap`) bounds how many
in-scope chats a search loads before ranking, so a large tenant can't blow memory.

**Tenant fold — the exact seam.** `SessionEmbedSearch` takes the SAME
`SessionFilter` as `ListSessions` and applies the owner/tenant fold in the SQL
`WHERE` — on the denormalised `session_embeddings.{tenant_id,user_id,agent}`
columns (copied from the authoritative session row at upsert; they are immutable
session facts) for the owner axes, and on `sessions.archived_at` for the archived
filter. A cross-tenant chat therefore **never leaves the DB**, even when its
vector is the closest possible match — the fold beats similarity. `related`
builds that filter via the same `filterForScope` the other reads use, and folds
the by-id source first through `loadSessionInScope`.

## Redaction

Transcripts are persisted **already-redacted** — the recording emit path
(`makeRecordingEmit`) scrubs secrets out of tool-call inputs and tool-result text
at write time (RFC Z / the `redact` context-transform). So `get` returns scrubbed
content without re-applying the transform; a secret that redaction would mask
never reaches a History reader.

## Transports

- **In-loop:** an agent with `tools: [History]` + a `history_scope` calls it like
  any built-in.
- **Off-run:** `POST /v1/_history` (bearer-authed, `ScopeTenant`; admin
  satisfies), the LoomCycle MCP meta-tool `history`, the gRPC RPC, and the
  TS/Python adapter twins. All resolve owner-scope + tenant from the
  authenticated principal, never the wire.
- `related` is a new **op** on that existing tool — it rides the same
  op-discriminated dispatch and JSON envelope (`session_id` / `query` / `limit`
  fields already on the schema), so every transport gains it with no wire change.

## Continuing a chat

History reads, annotates, and recaps chats — it does not itself start runs. To
*continue* a chat, issue a `POST /v1/runs` with its `session_id` (loomcycle's
existing session-continuation path); `resume` hands you exactly those coordinates
(`session_id` + `agent`) plus a hint, so a UI or agent can offer a one-click
continue without duplicating the run-trigger surface inside History.
