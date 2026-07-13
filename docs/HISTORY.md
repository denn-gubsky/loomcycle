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
  satisfies) and the LoomCycle MCP meta-tool `history`. Both resolve owner-scope
  + tenant from the authenticated principal, never the wire.
- The gRPC RPC and the TS/Python adapter twins are a later PR.

## Continuing a chat

History reads and annotates chats — it does not itself start runs. To *continue*
a chat, issue a `POST /v1/runs` with its `session_id` (loomcycle's existing
session-continuation path).
