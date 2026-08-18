# Skybits — shared document workspace via MCP

[Skybits](https://skybits.ai) is a shared document workspace for people **and** AI agents. Documents live on Skybits, are addressed by a `doc_url`, and are exposed to agents through a single remote MCP server. This guide covers wiring loomcycle to it: the MCP client config, the three auth bindings, and — the part that surprises people — how Skybits relates (and doesn't) to loomcycle Memory.

No loomcycle code change is required anywhere in this integration. Everything below is operator yaml plus per-run caller fields.

## What Skybits is

A Skybits workspace holds documents that humans and agents co-edit. The public tool catalog (`GET https://skybits.ai/v1/tools`) advertises 59 tools, the ones you'll reach most often:

- **Reading** — `read_document`, `outline_document`, `find_in_document`, `find_documents`.
- **Writing** — `create_doc`, `edit_document` (atomic batch ops: `replace` / `insert_before` / `insert_after` / `remove` / `move`), comment threads, tags, sharing, sync graph, export jobs.
- **Versioning** — `document_history`, `diff_document`, `restore_document`.

Two properties to internalize before prompting an agent against it:

1. **Documents are addressed by `doc_url`.** Every read/edit tool takes one. The agent's job is to discover and remember the right `doc_url`s (see § Memory interplay).
2. **There is no "suggestions" tool.** `edit_document` applies edits directly. Safety comes from attribution and versioning, not from a review queue: every edit is attributed to the token that made it, versioned, and revertible via `document_history` / `diff_document` / `restore_document`. If you want a human-in-the-loop posture, prompt the agent to diff before and after, or to post a comment thread summarizing its edits — don't wait for a suggest/approve API that doesn't exist.

## Architecture

```
┌──────────────┐   POST /v1/runs    ┌──────────────────┐   tools/call    ┌───────────────────┐
│  app server  │──user_credentials──▶│     loomcycle    │──JSON-RPC──────▶│  skybits.ai/mcp   │
│              │   or $cred: / env  │  (MCP client,    │  over Streamable │  (59 tools,       │
│              │   token fallback   │   zero Go code)  │  HTTP           │   OAuth-checked)  │
└──────────────┘                    └──────────────────┘◀──result────────└───────────────────┘
```

loomcycle consumes Skybits through its existing **MCP HTTP (Streamable) client** — `internal/tools/mcp/http/client.go`. At boot the pool (`internal/tools/mcp/pool.go`) runs `initialize` + `tools/list` against `https://skybits.ai/mcp` and registers each tool as `mcp__skybits__<tool>`. Per request, `Client.do()` substitutes `${...}` tokens into the outbound headers against the run's identity (`${run.user_bearer}`, `${run.credentials.<name>}`), a CredentialDef (`$cred:<name>`), or an operator env var (`${LOOMCYCLE_*}`) — the model never sees the token in any form. The full pipeline (substitution forms, drop semantics, model-visibility boundary, sub-agent inheritance) is documented in [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md); per-run named credentials in the `per-run-credentials` help topic; `$cred:` in [`docs/CREDENTIALS.md`](CREDENTIALS.md).

## Auth — three ways to bind a token

Skybits accepts two credential shapes at its MCP endpoint: **connector API keys** (plain Bearer, long-lived, minted for autonomous agents) and **OAuth 2.1 access tokens** (per-user, for interactive use). Loomcycle binds either through header substitution.

**State this plainly once: loomcycle's MCP client has no OAuth authorization-code or refresh flow.** Tokens are provisioned out-of-band — by Skybits' helper script, by your app server, or by the user — and loomcycle forwards whatever it is given verbatim. Refresh is the caller's responsibility; a stale or revoked token surfaces as a clean HTTP 401 tool error the model can see and report.

Skybits' OAuth side, for the caller that provisions tokens: protected-resource discovery at `https://skybits.ai/.well-known/oauth-protected-resource/mcp`, authorization server `https://auth.skybits.ai`, with Dynamic Client Registration, PKCE S256, and the device-code grant.

### Option A — connector API key (recommended for autonomous agents)

Connector keys are the right fit for scheduled runs, sub-agents, and anything without a human at the keyboard. Mint one via the device flow using Skybits' public helper:

```sh
# from https://skybits.ai/skill
python scripts/skybits.py login <connector-name>
```

or directly against the API with the scopes loomcycle agents need:

```http
POST https://skybits.ai/v1/mcp/connectors
{ "scopes": ["mcp.connect", "agent.tools.read", "agent.tools.write"] }
```

Then bind the key either as an **operator env var**:

```yaml
mcp_servers:
  skybits:
    transport: http
    url: https://skybits.ai/mcp
    headers:
      Authorization: "Bearer ${LOOMCYCLE_SKYBITS_TOKEN}"
```

or as a **`$cred:skybits` CredentialDef** ([`docs/CREDENTIALS.md`](CREDENTIALS.md)) so a tenant/user's own key shadows the operator default:

```yaml
mcp_servers:
  skybits:
    transport: http
    url: https://skybits.ai/mcp
    headers:
      Authorization: "Bearer $cred:skybits"   # scope precedence agent > user > tenant
```

### Option B — per-user OAuth access token via `user_credentials` (interactive use)

Your app server runs the OAuth flow against `auth.skybits.ai` (DCR + PKCE S256, or device-code for CLI-shaped clients), then passes the resulting access token on each run:

```http
POST /v1/runs
{
  "agent": "doc-assistant",
  "user_id": "u_42",
  "user_credentials": { "skybits": "<user's-skybits-access-token>" },
  "segments": [...]
}
```

```yaml
mcp_servers:
  skybits:
    transport: http
    url: https://skybits.ai/mcp
    headers:
      Authorization: "Bearer ${run.credentials.skybits}"
```

The bare `${run.credentials.skybits}` form is strict: a run that carries no `skybits` credential gets the header **dropped**, and Skybits' 401 comes back as a typed tool error — far more debuggable than a literal `${...}` string sent downstream. When the token expires mid-session, the caller refreshes it and passes the new one on the next run; loomcycle never refreshes it for you.

### Option C — combined: per-user credential with an env fallback

Useful during rollout — users with a linked Skybits account act as themselves, everyone else falls back to a connector key:

```yaml
mcp_servers:
  skybits:
    transport: http
    url: https://skybits.ai/mcp
    headers:
      Authorization: "Bearer ${run.credentials.skybits:-${LOOMCYCLE_SKYBITS_TOKEN}}"
```

The inner `${LOOMCYCLE_SKYBITS_TOKEN}` resolves at config load; the outer `${run.credentials.skybits:-...}` resolves per request, so one pooled server serves both shapes. (`${run.user_bearer}` works too if your deployment already funnels a single per-user token through that field — Option B with the named-credential key is just the more explicit form.)

## Configuration

The full static block:

```yaml
# loomcycle.yaml
mcp_servers:
  skybits:
    transport: http
    url: https://skybits.ai/mcp
    headers:
      Authorization: "Bearer ${run.credentials.skybits:-${LOOMCYCLE_SKYBITS_TOKEN}}"
    # Optional operator-level filter — register ONLY these tools even
    # though Skybits advertises 59. Per-agent tools: narrows further
    # on top of this.
    tools: [read_document, outline_document, find_documents, find_in_document,
            create_doc, edit_document, document_history, diff_document,
            restore_document]
```

Agents opt in by name or glob — nothing is exposed by default:

```yaml
agents:
  doc-assistant:
    tools:
      - mcp__skybits__*        # or list individual mcp__skybits__<tool> names
      - Memory                 # see § Memory interplay
    memory_scopes: [user, agent]
```

**Dynamic alternative:** if you'd rather not restart to add the server, register an `MCPServerDef` at runtime via `POST /v1/_mcpserverdef`. The body takes `name` at the top level and the transport settings nested under `overlay` (`promote` defaults to `true` on create):

```json
{
  "op": "create",
  "name": "skybits",
  "overlay": {
    "transport": "http",
    "url": "https://skybits.ai/mcp",
    "headers": { "Authorization": "Bearer ${LOOMCYCLE_SKYBITS_TOKEN}" }
  }
}
```

One requirement to know up front: for dynamic registration the URL's hostname must already be in the operator's `LOOMCYCLE_HTTP_HOST_ALLOWLIST` — SSRF defence at the registration boundary — so `skybits.ai` has to be allowlisted before this POST succeeds. The gate applies to dynamic registration only; a static yaml `mcp_servers:` entry like the ones above needs no allowlist entry. Details in the `dynamic-mcp` help topic.

## Memory interplay

This is the section to read before pointing a long-running agent at Skybits.

**MCP tool calls are not auto-recorded in memory.** Loomcycle's memory consolidation banks **dialogue only** — user and assistant text; tool calls and tool results are filtered out by block type (see [`docs/CONFIGURATION.md`](CONFIGURATION.md) § `memory_flush`). An agent can spend fifty turns editing Skybits documents and consolidate *nothing* about it. If you want durable facts — which `doc_url` is the spec, how the workspace is structured, what was decided — the agent must write them explicitly. That's why the agent above carries `Memory` in `tools` plus `memory_scopes: [user, agent]` (Memory is default-deny without scopes). Prompt it to save facts as it goes:

```
When you create or discover a Skybits document, save its doc_url and purpose
via Memory op=set (e.g. key "skybits:doc:api-spec"). When you make a structural
decision about the workspace, record it via Memory op=add.
```

Facts written this way land tenant-isolated in the loomcycle memory store and come back on later runs — but mind the ops: on the default SQLite/inprocess backend, `Memory op=list` / `op=get` with a key prefix work without any vector stack, while `op=recall` / `op=search` need a configured embedder + a vector-capable backend. (That's why the exp11 example sticks to `list` / `get` / `set`.)

**Skybits content lives outside loomcycle's memory/document keyspace.** Loomcycle's own Document primitive stores chunks under the reserved `doc.chunk:` key prefix, and a `Memory op=search` with `sources: [documents]` searches exactly that prefix — it never sees Skybits documents. Retrieval of Skybits content always goes through `mcp__skybits__find_documents` / `mcp__skybits__read_document` against the live workspace. Don't try to close the gap by mirroring Skybits docs into loomcycle memory; treat loomcycle memory as the index of *pointers and decisions* and Skybits as the content store.

**Skybits cannot be a loomcycle `memory_backend`.** The MemoryBackend substrate has exactly two kinds — `inprocess` and `remote` (a peer *loomcycle* instance, RFC CD) — and a `remote` backend proxies loomcycle's six memory ops to a peer's `/v1/_memory/*`, which Skybits does not implement. See [`docs/MEMORY-BACKENDS.md`](MEMORY-BACKENDS.md).

**The inverse path exists.** Loomcycle is itself an MCP server ([`docs/MCP_SERVER.md`](MCP_SERVER.md)) — a Skybits-side agent (or any MCP client) can be pointed at `loomcycle mcp` to read the facts your loomcycle agents banked about the workspace. The two systems stay in sync by each remembering its own side, not by one pretending to be the other's store.

## Runnable example

[`examples/exp11-skybits-documents/`](../examples/exp11-skybits-documents/) — an end-to-end agent that authenticates with a connector key, discovers and edits Skybits documents, and banks `doc_url`s into loomcycle memory.

## Cross-references

- [`docs/MCP_INTEGRATION.md`](MCP_INTEGRATION.md) — the full MCP HTTP pipeline: substitution forms, drop semantics, the model-visibility boundary table, failure modes.
- [`docs/CREDENTIALS.md`](CREDENTIALS.md) — the CredentialDef store and `$cred:<name>` consumption.
- [`docs/CONFIGURATION.md`](CONFIGURATION.md) — `memory_scopes`, `memory_flush`, consolidation semantics.
- [`docs/MEMORY-BACKENDS.md`](MEMORY-BACKENDS.md) — the two memory-backend kinds.
- [`docs/MCP_SERVER.md`](MCP_SERVER.md) — loomcycle as an MCP server (the inverse direction).
- Skybits: `https://skybits.ai` · MCP endpoint `https://skybits.ai/mcp` · tool catalog `GET https://skybits.ai/v1/tools` · OAuth discovery `https://skybits.ai/.well-known/oauth-protected-resource/mcp` · auth server `https://auth.skybits.ai` · connector helper `https://skybits.ai/skill`.
