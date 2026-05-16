# MCP integration — end-to-end pipeline

This guide explains how loomcycle's MCP HTTP integration works at every hop, so a third-party developer wrapping a REST API as an MCP server has a complete mental model before writing code. The primary downstream consumer (`jobs-search-agent`) is referenced throughout as a worked example.

Scope: **MCP HTTP (Streamable) transport**, on the **client side** (loomcycle consuming an MCP server). stdio MCP and loomcycle's own role as an MCP server (the `loomcycle mcp` subcommand, v0.8.15+) are out of scope.

---

## 0. Why MCP, not the built-in HTTP tool?

Loomcycle ships a built-in `HTTP` tool (`internal/tools/builtin/http_tool.go`). Why not just point the model at your REST API directly and skip MCP entirely?

**Two anchors: security and structure.**

### Security: the bearer-leak surface

The built-in HTTP tool takes `{url, method, headers, body}` as model-generated input args. If you want the model to call an authenticated REST endpoint, the bearer token has to appear somewhere the model can produce it — typically by putting it in the system prompt so the model can paste it into the `headers` arg. That makes the bearer **model-visible everywhere**:

- It appears in the system prompt that gets sent to the LLM provider (Anthropic / DeepSeek / etc.)
- The model can echo it in subsequent assistant turns ("Let me try that again with `Bearer abc123…`")
- The tool-call event recorded in `events` table contains it verbatim (model-generated input args)
- Operator transcripts and SSE replays show it
- Any downstream observability tool sees it

MCP inverts this. The bearer lives **only** in operator yaml as a substitution template:

```yaml
mcp_servers:
  myapi:
    headers:
      Authorization: "Bearer ${run.user_bearer}"
```

Loomcycle substitutes it into the outbound HTTP header at request-build time. The model **never sees the bearer in any form** — not in the prompt, not in the tool description, not in the input args it generates, not in the tool result. See § 3's boundary table.

### Structure: typed schemas vs free-form HTTP

The HTTP tool's input schema is the four fields above. The model has to construct a valid REST call from whatever the system prompt told it about your API: which path, which method, which query params, which content-type, which body shape. Tool-trained LLMs (Claude 4+, GPT-5+, etc.) are good at this but not perfect — hallucinated paths, wrong query params, the wrong content-type on PATCH, missing required fields, etc.

MCP shifts the contract. Your server's `tools/list` response exposes each operation as a **named, typed tool** with its own JSON Schema:

```
mcp__myapi__patchApplication(applicationId: string, status: string, notes?: string)
mcp__myapi__getAgentContext(scope: "user" | "team")
mcp__myapi__postSearchIngest(payload: SearchPayload)
```

The model picks a tool by name and fills typed parameters. No URL construction. No header juggling. No body-shape guessing. Models are reliably better at "fill these typed parameters" than at "compose this HTTP call against an API I've only seen described in the system prompt." Empirically: tool-trained LLMs almost never hallucinate parameter names or types against a well-formed MCP schema, but they routinely hallucinate REST paths and query-string conventions.

### One-sentence summary

**MCP gives the model a typed, auth-free tool surface; loomcycle does the unsafe work (bearer injection, transport, schema validation) outside the model's view.** Every mechanism in the lifecycle trace below delivers on this promise.

### Honest caveats

- The HTTP tool still has a place for one-off public-API calls where running an MCP server is overkill (fetching a single webpage, hitting a no-auth public endpoint).
- MCP adds operational surface area: an extra HTTP server you maintain, an extra hop per call, an extra failure mode (server unreachable at boot). The v0.8.1+ lazy-retry path mitigates the cold-start case but doesn't eliminate the operational tax.
- For prototyping, the HTTP tool's flexibility is a feature. For production agent-to-API integration, MCP is the right pick.

---

## 1. Overview

```
┌──────────────┐   POST /v1/runs    ┌────────────────────┐   tools/call   ┌──────────────────┐
│  app server  │──user_bearer──────▶│      loomcycle     │───JSON-RPC───▶│   MCP server     │
│              │   (the run req)    │  (HTTP+SSE API     │   over HTTP    │  (your service)  │
│  e.g.        │                    │   + agent loop)    │                │                  │
│  jobs-       │                    │                    │◀──response────│  (forwards to    │
│  search-web  │◀────SSE events─────│  ╭─────────────╮   │   (JSON-RPC    │   your internal  │
└──────────────┘                    │  │  LLM via    │   │    or SSE)     │   REST API)      │
                                    │  │  provider   │   │                │                  │
                                    │  │  (Anthropic │   │                └──────────────────┘
                                    │  │   etc.)     │   │
                                    │  ╰─────────────╯   │
                                    └────────────────────┘
```

A run begins when the app server POSTs to `/v1/runs`. The body carries a `user_bearer` field along with the usual agent/segments/user_id. Loomcycle attaches that bearer to ctx, runs the agent loop, and when the LLM emits a tool_call named `mcp__<server>__<tool>`, loomcycle's MCP HTTP client substitutes the bearer into the outbound `Authorization` header per the operator yaml template, calls your MCP server, parses the JSON-RPC response, and folds it back into the conversation as a tool_result content block. The LLM continues on the next turn. The app server sees the whole thing as an SSE stream of typed events.

---

## 2. Request lifecycle, stage by stage

### 2.1 Caller submits a run

The app server (e.g., `jobs-search-web`) sends:

```http
POST /v1/runs HTTP/1.1
Authorization: Bearer <LOOMCYCLE_AUTH_TOKEN>
Content-Type: application/json

{
  "agent": "employer-profiler",
  "user_id": "u_42",
  "user_tier": "medium",
  "user_bearer": "eyJhbGciOi...",      ← per-user, per-run token (NEW v0.8.14)
  "segments": [
    { "role": "user", "content": [{ "type": "trusted-text", "text": "..." }] }
  ]
}
```

The `user_bearer` field is **optional**. Validation lives in `validUserBearer()` at `internal/api/http/server.go:2838` — accepted format is `[A-Za-z0-9._\-+/=]{16,512}`, which covers JWT base64url alphabets and most opaque token schemes. Empty is allowed for back-compat (callers with static-bearer-only configs need no change).

The wire field is defined on `runRequest` at `internal/api/http/server.go:1233`. A parallel field exists on `messagesRequest` for `/v1/messages/{session_id}` continuations at `:1589` — same shape, same validation.

The `LOOMCYCLE_AUTH_TOKEN` header is loomcycle's own auth (operator-managed). It's **independent** of `user_bearer`: the operator token authorizes the app server to talk to loomcycle; the `user_bearer` is what loomcycle forwards to YOUR MCP server. Two different trust boundaries.

### 2.2 Loomcycle attaches identity to ctx

At run start, the HTTP handler builds a `tools.RunIdentityValue` and stitches it into the loop's ctx:

```go
loopCtx = tools.WithRunIdentity(loopCtx, tools.RunIdentityValue{
    UserID:     effectiveUserID,
    AgentID:    agentID,
    UserTier:   in.UserTier,
    UserBearer: in.UserBearer,      // v0.8.x: per-run MCP bearer
})
```

— `internal/api/http/server.go:810`

The `RunIdentityValue` struct lives at `internal/tools/tool.go:163` and carries five fields: `UserID`, `AgentID`, `UserTier`, `AgentDefID`, `UserBearer`. The getter is `tools.RunIdentity(ctx) → RunIdentityValue` at `:203`. Any tool downstream — including the MCP HTTP client — can recover these by calling `tools.RunIdentity(ctx)`.

The setter fires from **four places** (mirroring the four CreateRun sites in `server.go`): `handleRuns`, `handleMessages` continuation, `RunOnce` (the agent-runner entry), and `runSubAgent` (the Agent-tool spawn path). All four read `UserBearer` from the request body and attach it identically.

### 2.3 Agent loop runs

The agent loop iterates `model → tool_use → tool_result → model` against the LLM provider. When the LLM emits an `EventToolCall` with `tu.Name = "mcp__jobs__getAgentContext"`, the loop dispatches it through the tool dispatcher (`internal/loop/loop.go` around `:830`). The dispatcher looks the name up in its registry, finds the MCP tool wrapper, and calls its `Execute(ctx, input) → tools.Result`.

Critically, the same `ctx` that the loop received from the HTTP handler (with `RunIdentityValue` attached) flows straight into the tool. No re-wrapping, no copying. The MCP client below recovers `UserBearer` from this ctx at request-build time.

### 2.4 MCP HTTP client builds the outbound request

The MCP client (`internal/tools/mcp/http/client.go`) speaks MCP's Streamable HTTP transport. For each tool call, it POSTs a JSON-RPC envelope to the configured URL:

```http
POST https://api.example.com/mcp HTTP/1.1
Content-Type: application/json
Accept: application/json, text/event-stream      ← both required by spec
Mcp-Session-Id: <id-from-initialize-response>
Authorization: Bearer <substituted-from-${run.user_bearer}>

{
  "jsonrpc": "2.0",
  "id": "...",
  "method": "tools/call",
  "params": { "name": "getAgentContext", "arguments": {} }
}
```

The `Accept` header lists **both** JSON and SSE — strict servers (e.g., the official `@modelcontextprotocol/sdk`'s `WebStandardStreamableHTTPServerTransport`) return HTTP 406 otherwise. This was a v0.4.x hardening point. Loomcycle parses whichever the server replies with (`internal/tools/mcp/http/client.go:140`, `extractSSEData` at `:263`).

The per-request header construction is at `client.go:204–244`. The substitution loop:

```go
runIdent := tools.RunIdentity(ctx)
for k, v := range c.headers {
    subV, drop := substituteRunVars(v, runIdent.UserBearer)
    if drop {
        log.Printf("mcp http: ${run.user_bearer} unresolved for header %q on %q (agent_id=%s, bearer=%s); dropping header",
            k, c.url, runIdent.AgentID, tokenPrefix(runIdent.UserBearer))
        continue
    }
    req.Header.Set(k, subV)
}
```

— `client.go:227–237`

Three properties to notice:

1. **The substitution is per-request, never against `c.headers` in-place.** `c.headers` is shared across all concurrent runs that go through this Client (the Client is per-server, not per-run). If we mutated it, run A's bearer would leak into run B's request milliseconds later. The local-copy invariant is load-bearing.

2. **`drop=true` means "the operator declared `${run.user_bearer}` strictly, but the run carried no bearer."** Rather than send a literal `Bearer ${run.user_bearer}` downstream (which would look like a 200-with-wrong-user from the server's perspective and produce confusing errors), loomcycle drops the header entirely. The MCP server's own auth check then returns a clean 401, which surfaces as a typed tool error — far more debuggable.

3. **The log line uses `tokenPrefix()`** (`internal/tools/mcp/http/substitute.go:61`) — only the first 4 chars + ellipsis. **Full tokens are never logged**, even on the WARN path. This is the only place a bearer touches logs.

### 2.5 MCP server responds

Your MCP server processes the call and replies. Two valid shapes (`client.go:99–162`):

**JSON-RPC** (200/202 with `Content-Type: application/json`):

```json
{
  "jsonrpc": "2.0",
  "id": "...",
  "result": { "content": [{ "type": "text", "text": "..." }] }
}
```

**SSE-framed** (200 with `Content-Type: text/event-stream`):

```
data: {"jsonrpc":"2.0","id":"...","result":{"content":[{"type":"text","text":"..."}]}}

```

Loomcycle's `extractSSEData()` peels off the `data:` prefix and parses the embedded JSON. Servers that conform to the Streamable HTTP spec pick per-request based on response shape; loomcycle handles either. The bare JSON shape is fine for short responses; SSE is useful when the server wants to stream progress events back (loomcycle currently only consumes the final `result` frame, but the transport is open for future streaming).

**Session id**: the server may include `Mcp-Session-Id: <id>` in the initial `initialize` response. Loomcycle captures it (`client.go:107`) and echoes it on every subsequent request. If the server returns **404** mid-session (`client.go:118`), loomcycle marks the client dead (`c.dead=true`) and the pool evicts it; the next call to the same server triggers a fresh handshake.

### 2.6 Loop folds result back into the conversation

The MCP tool wrapper returns `tools.Result{Text, IsError}` to the dispatcher. The loop's `executePendingTools` collects all in-flight tool results, emits an `EventToolResult` event for each (`internal/loop/loop.go:844`), and assembles them into `tool_result` content blocks for the next provider call:

```go
results[r.idx] = providers.ContentBlock{
    Type:      "tool_result",
    ToolUseID: r.tu.ID,
    ToolName:  r.tu.Name,
    Text:      r.res.Text,
    IsError:   r.res.IsError,
}
```

— `loop.go:856`

The next turn's request to the LLM includes these blocks, and the model produces its next assistant turn. The event also persists to the `events` table for transcript replay and the Web UI's run-detail view. Nothing strips or rewrites the result text on the way in — the MCP server's response body is what the model sees.

---

## 3. Bearer token pipeline (deep dive)

### Who generates the token

The **caller** (typically your app server). Loomcycle treats it as an opaque string and forwards it verbatim. Two patterns we've seen in production:

1. **Per-user, per-session token** — your app issues a token bound to the authenticated user, scoped to a session, with a TTL. Pass it as `user_bearer` on the run request. Your MCP server validates it via your existing auth flow. This is what jobs-search-agent does.

2. **Static operator-shared token** — fall back to a single shared token via the `${run.user_bearer:-FALLBACK}` form. Useful during rollout when most callers haven't been updated to send a per-user token yet. Once rollout completes, drop the fallback.

### Substitution forms

Both work in any header value, anywhere in operator yaml:

| Form | Behaviour when `user_bearer` is empty |
|---|---|
| `${run.user_bearer}` | **Header dropped.** Logged at WARN with a 4-char token prefix. MCP server returns 401, model sees a typed tool error. |
| `${run.user_bearer:-FALLBACK}` | Replaces with the literal text `FALLBACK`. Useful for static-token fallback during rollout. |

Both forms work inline anywhere in a header value, not just as the whole value. So `"Authorization: Bearer ${run.user_bearer}"` and `"X-Custom-Header: prefix-${run.user_bearer}-suffix"` both work. The matcher is the regex at `internal/tools/mcp/http/substitute.go:19`:

```go
var runBearerRe = regexp.MustCompile(`\$\{run\.user_bearer(?::-(.*?))?\}`)
```

### Substitution flow

```
       Operator yaml header template:
       ┌─────────────────────────────────────────────┐
       │ headers:                                    │
       │   Authorization: "Bearer ${run.user_bearer}"│
       └────────────────────┬────────────────────────┘
                            │ parsed at config-load (once)
                            ▼
                   c.headers (Client field)
                   {"Authorization": "Bearer ${run.user_bearer}"}
                            │
                            │ shared across all runs that use this server
                            │
   Run A's ctx ──┐          │          ┌── Run B's ctx
   (bearer=X)    │          │          │   (bearer=Y)
                 ▼          ▼          ▼
            ┌────────────────────────────────────┐
            │  Client.do(ctx, body)              │
            │  ┌──────────────────────────────┐  │
            │  │ runIdent := RunIdentity(ctx) │  │
            │  │ for k, v := range c.headers: │  │
            │  │   subV := substitute(v,      │  │
            │  │            runIdent.Bearer)  │  │
            │  │   req.Header.Set(k, subV)    │  │
            │  └──────────────────────────────┘  │
            │  ↑ local copy — never mutates      │
            │    c.headers; safe under concurrency │
            └────────────────────────────────────┘
                            │              │
                            ▼              ▼
                    "Bearer X"     "Bearer Y"
                    (Run A wire)   (Run B wire)
```

The yaml template is parsed once at config-load and stored on the Client. The substitution happens **per request**, against a per-request copy of the headers map. Two concurrent runs against the same MCP server hit `Client.do` simultaneously, each with their own ctx, each producing their own outbound `Authorization` header. The shared `c.headers` is never mutated.

### What the model can see — boundary table

This is the load-bearing security property. The bearer appears on **exactly one** surface and is absent everywhere else:

| Surface | Bearer visible? | Why |
|---|:---:|---|
| Outbound MCP HTTP header | ✅ | by design — that's the point of the substitution |
| LLM system prompt | ❌ | bearer is never substituted into prompt content; substitution only fires in `Client.do` |
| Tool descriptions (`tools/list`) to LLM | ❌ | tool schemas are declared by the MCP server, static, do not reference the bearer |
| Tool input args (model-generated) | ❌ | the model generates `tools/call` args; the bearer is in the HTTP envelope, not the JSON body |
| Tool result text | ❌ | MCP server's response body is application-level; HTTP request headers don't appear in the response |
| Process logs | partial | only `tokenPrefix()` (first 4 chars + ellipsis), only on the drop-warning path |
| SQLite `events` store | ❌ | events store tool_call inputs (model-generated) + tool_result text — neither contains the bearer |
| SSE events to the caller | ❌ | the SSE stream replays `events` table content — same exclusion |

To break this property a developer would have to either:
- Put the bearer in the agent's system prompt (don't do that — defeats the entire point)
- Have the MCP server reflect the `Authorization` header back in its response body (that's a bug in the MCP server, not loomcycle)
- Log the full ctx somewhere — but the only access path is `tools.RunIdentity(ctx)` and the only call site that uses `UserBearer` is `Client.do`, which uses `tokenPrefix()` on the log path

### Sub-agent inheritance

When an agent calls the `Agent` built-in to spawn a sub-agent, the parent's `RunIdentityValue` is propagated **verbatim** to the child:

```go
subCtx = tools.WithRunIdentity(subCtx, tools.RunIdentityValue{
    UserID:     parentIdentity.UserID,
    AgentID:    subAgentID,                    // fresh child ID
    UserTier:   parentIdentity.UserTier,       // same end-user tier
    AgentDefID: defID,                         // pinned by parent
    UserBearer: parentIdentity.UserBearer,     // same end-user bearer
})
```

— `internal/api/http/server.go:2324–2330`

The sub-agent acts on behalf of the same end-user. When the sub-agent's loop calls an MCP tool, the substitution in `Client.do` reads its ctx and gets the parent's bearer — automatically, no re-wiring. Sub-agent trees of arbitrary depth all share the originating user's bearer.

```
        Parent run (user_bearer = X)
        │
        │ ctx carries X
        ▼
        ┌─────────────────┐
        │ Agent tool      │  ── spawns sub-agent
        │ dispatched      │     subCtx = WithRunIdentity(...{UserBearer: X})
        └────────┬────────┘
                 ▼
        Sub-agent loop
        │
        │ ctx still carries X
        ▼
        ┌─────────────────┐
        │ MCP tool call   │  ── Client.do reads X from sub-agent's ctx,
        │ dispatched      │     substitutes into outbound header
        └────────┬────────┘
                 ▼
        MCP server sees:
        Authorization: Bearer X
        (same user as parent)
```

This works for arbitrary depth (sub-sub-agent etc.). If you need to **narrow** the bearer per sub-agent (e.g., scope to a different user/role), that's not currently supported — file an issue.

---

## 4. How `user_id` is identified across the pipeline

`user_id` is **separate** from `user_bearer`. Both are caller-supplied opaque strings, but they play different roles:

| | `user_id` | `user_bearer` |
|---|---|---|
| Set by caller | yes | yes |
| Validated by loomcycle | format only (`[A-Za-z0-9_-]{1,128}`) | format only (`[A-Za-z0-9._\-+/=]{16,512}`) |
| Persisted | `runs.user_id` column | not persisted |
| Queryable | `GET /v1/users/{user_id}/agents` | n/a |
| Forwarded to MCP servers | **no** | **yes** (via substitution) |
| Visible to LLM | no | no |
| Purpose | grouping / observability | authorizing MCP-server calls |

The asymmetry matters: `user_id` is for **identifying which run belongs to which user** (so operators can list a user's active runs, scope cancellations, build per-user dashboards). `user_bearer` is for **authorizing the MCP server's REST forwarding** back to the app — your MCP server validates this bearer against your auth registry, resolves the actual user, and authorizes the call.

In practice they're related (the bearer encodes the same user as `user_id`, modulo TTL/scope), but loomcycle treats them as independent fields. Pass both. Your MCP server should treat the bearer as authoritative (it carries cryptographic guarantees; `user_id` is just a label).

---

## 5. Recipe: wrapping your REST API as an MCP server

A minimal viable integration:

### Step 1: Run an MCP Streamable HTTP server

Use the official SDKs:
- TypeScript: `@modelcontextprotocol/sdk` → `WebStandardStreamableHTTPServerTransport`
- Python: `mcp` package
- Go: pick a community implementation or roll your own against the spec

Spec: https://modelcontextprotocol.io/docs/concepts/transports — the Streamable HTTP section is what loomcycle speaks.

### Step 2: Expose your operations as MCP tools

Each REST endpoint maps to a typed tool. Define the tool's `inputSchema` carefully — that's what the LLM sees and fills:

```typescript
server.tool(
  "patchApplication",
  "Update an application's status and notes.",
  {
    applicationId: z.string(),
    status: z.enum(["draft", "submitted", "withdrawn"]),
    notes: z.string().optional(),
  },
  async ({ applicationId, status, notes }, { authInfo }) => {
    // authInfo carries the bearer your handler validated at request entry
    const result = await fetch(`/api/applications/${applicationId}`, {
      method: "PATCH",
      headers: { Authorization: `Bearer ${authInfo.bearer}` },
      body: JSON.stringify({ status, notes }),
    });
    return { content: [{ type: "text", text: await result.text() }] };
  }
);
```

### Step 3: Validate the inbound bearer

At your route handler's entry point, extract the `Authorization` header, validate it against your auth registry, and reject with 401 on failure. Loomcycle treats 401 as a clean tool-error — the model sees it and can adjust (or fail, if it's a transient).

```typescript
const authHeader = request.headers.get("Authorization");
if (!authHeader?.startsWith("Bearer ")) {
  return new Response(JSON.stringify({ error: "Authorization required" }),
    { status: 401, headers: { "Content-Type": "application/json" } });
}
const bearer = authHeader.slice("Bearer ".length).trim();
const userId = validateApiToken(bearer);
if (!userId) {
  return new Response(JSON.stringify({ error: "Invalid bearer" }),
    { status: 401, headers: { "Content-Type": "application/json" } });
}
```

### Step 4: Register the server in loomcycle's operator yaml

```yaml
mcp_servers:
  myapi:
    transport: http
    url: https://api.example.com/mcp
    headers:
      Authorization: "Bearer ${run.user_bearer}"
    # Optional: operator-level filter — registers ONLY these tools
    # even if your server advertises more. Per-agent allowed_tools
    # narrows further on top of this.
    allowed_tools: [patchApplication, getAgentContext]
```

Restart loomcycle. At startup, loomcycle's pool (`internal/tools/mcp/pool.go:46`) connects, runs `initialize` + `tools/list`, applies `allowed_tools` if set, and registers each tool as `mcp__myapi__<toolName>`. The `mcp__server__tool` naming is at `pool.go:293`; `sanitiseServerName` (`:343`) handles edge cases like spaces in server names.

### Step 5: Allow the tools per agent

Agents only see tools they've explicitly allowed. In the agent's frontmatter:

```yaml
---
name: my-agent
tier: middle
allowed_tools:
  - mcp__myapi__patchApplication
  - mcp__myapi__getAgentContext
---
```

### Step 6: Caller passes `user_bearer` on every relevant run

Your app server, when submitting a run that should authenticate against your MCP server as the end-user:

```json
POST /v1/runs
{
  "agent": "my-agent",
  "user_id": "u_42",
  "user_bearer": "<token-your-MCP-server-validates>",
  "segments": [...]
}
```

### Failure modes

| Symptom | Cause | What to do |
|---|---|---|
| Model sees tool error "authentication failed" or HTTP 401 | bearer is stale/invalid | refresh on the caller side; pass a fresh `user_bearer` |
| Loomcycle log: `mcp http: ${run.user_bearer} unresolved for header...` | caller forgot to pass `user_bearer` and you used the strict form | either pass the bearer or switch yaml to the `:-FALLBACK` form |
| MCP server tool not in registry at boot | server unreachable at startup; loomcycle logged "skipped" | first agent call that needs the tool triggers lazy retry (`internal/tools/mcp/lazy.go:60–182`) — peer restarts no longer require a loomcycle restart (v0.8.1+) |
| All MCP calls error mid-session with 404 | server invalidated the session; loomcycle marks the client dead | next call triggers a fresh handshake automatically — no action needed |
| Operator yaml `allowed_tools` filter excludes a tool | name mismatch | tool names are case-sensitive; check the server's `tools/list` output |

---

## 6. Worked example: `jobs-search-agent`

The reference implementation lives at `~/work/jobs-search-agent/jobs-search-web/src/app/api/mcp/route.ts` — a Next.js App Router route handler exposing 17 typed tools as `mcp__jobs__*`.

**Architecture:**

```
┌───────────────┐   POST /v1/runs    ┌─────────────┐   tools/call (loopback)   ┌─────────────────────┐
│ jobs-search-  │──user_bearer──────▶│  loomcycle  │──Authorization: Bearer───▶│ jobs-search-web's   │
│ web (Next.js) │                    │             │  ${run.user_bearer}       │ /api/mcp/route.ts   │
│               │                    │             │                           │                     │
│ generates     │                    │             │                           │ validateApiToken()  │
│ per-user      │                    │             │                           │   → userId          │
│ bearer        │                    │             │                           │                     │
└───────────────┘                    └─────────────┘                           │ tools forward the   │
                                                                               │ same bearer to     │
                                                                               │ /api/* endpoints   │
                                                                               │ (loopback fetch)   │
                                                                               └─────────────────────┘
```

**Key design choices in the jobs-search-agent route:**

1. **Single bearer auth.** Reject anything that isn't `Authorization: Bearer <token>`. Token is validated via the same `validateApiToken` registry every other agent-facing route uses — no special MCP-only auth path.

2. **Stateless MCP server.** Every request creates a fresh `McpServer + transport` (`sessionIdGenerator: undefined`). MCP allows session bookkeeping but tools-only flows don't require it. Cost: one constructor call + a tool-registration loop per request, negligible compared to LLM round-trip latency.

3. **Bearer captured at server construction, forwarded by tools.** The `createMcpServer({ bearer })` factory closes over the bearer; each tool's implementation uses it for loopback fetches against `jobs-search-web`'s own REST API. The MCP layer is essentially **transparent** — tools forward the same bearer to the same endpoints the user would hit interactively, so existing route auth logic just works.

4. **Co-located.** Loomcycle and jobs-search-web run on the same host. The MCP endpoint is `http://localhost:3000/api/mcp`. Latency is ~1 ms; no separate ops surface.

**Operator yaml fragment:**

```yaml
mcp_servers:
  jobs:
    transport: http
    url: http://localhost:3000/api/mcp
    headers:
      Authorization: "Bearer ${run.user_bearer}"
```

Tools registered: `mcp__jobs__getAgentContext`, `mcp__jobs__patchApplication`, `mcp__jobs__postSearchIngest`, and 14 others. Each is referenced by name in the relevant agent's `allowed_tools`.

**End-to-end:** jobs-search-web's user submits a run → POST /v1/runs with `user_bearer = <their per-user API token>` → loomcycle attaches to ctx → agent loop emits `mcp__jobs__patchApplication` tool call → MCP client substitutes the bearer into the `Authorization` header → jobs-search-web's `/api/mcp/route.ts` validates the token, resolves the user, runs the tool, which itself fetches a loopback PATCH on `/api/applications/...` with the same bearer → loopback PATCH authorizes as the same user → response bubbles back → loomcycle folds tool_result into the conversation → LLM continues.

The MCP layer is essentially invisible: the same bearer that authorizes the user's browser session ends up authorizing the LLM-driven PATCH.

---

## 7. Cross-references

- **`docs/ARCHITECTURE.md`** — overall runtime architecture. The "MCP integration" lines there describe BOTH the consumer side (this doc) and the v0.8.15+ self-as-server inversion (the `loomcycle mcp` subcommand); this doc is the consumer side only.
- **`CLAUDE.md`** — terse version of the MCP-HTTP hardening history (v0.8.0 → v0.8.14 evolution). Useful for context on why specific guards exist.
- **MCP spec**: https://modelcontextprotocol.io — Streamable HTTP transport, JSON-RPC envelopes, `tools/list` and `tools/call` semantics.
- **Code paths cited in this doc**: see the table in § 8 for a single index.

---

## 8. Code path index

Single jump-list of every file:line cited above, for readers who want to trace any claim back to source. As of v0.8.16 (post-PR-#116):

| What | Where |
|---|---|
| `runRequest.UserBearer` field | `internal/api/http/server.go:1233` |
| `messagesRequest.UserBearer` field | `internal/api/http/server.go:1589` |
| `validUserBearer()` validator | `internal/api/http/server.go:2838` |
| `WithRunIdentity` call at `handleRuns` | `internal/api/http/server.go:1506` |
| `WithRunIdentity` call at `RunOnce` | `internal/api/http/server.go:810` |
| `WithRunIdentity` call at `handleMessages` | `internal/api/http/server.go:1803` |
| `WithRunIdentity` call at `runSubAgent` | `internal/api/http/server.go:2324` |
| `tools.RunIdentityValue` struct definition | `internal/tools/tool.go:163` |
| `tools.WithRunIdentity` setter | `internal/tools/tool.go:195` |
| `tools.RunIdentity` getter | `internal/tools/tool.go:203` |
| MCP HTTP `Client.do()` substitution loop | `internal/tools/mcp/http/client.go:204–244` |
| `substituteRunVars()` engine | `internal/tools/mcp/http/substitute.go:39` |
| Substitution regex (`${run.user_bearer}` + fallback form) | `internal/tools/mcp/http/substitute.go:19` |
| `tokenPrefix()` log redactor | `internal/tools/mcp/http/substitute.go:61` |
| MCP tool naming (`mcp__server__tool`) | `internal/tools/mcp/pool.go:293` |
| `sanitiseServerName()` | `internal/tools/mcp/pool.go:343` |
| Pool startup + handshake | `internal/tools/mcp/pool.go:46–158` |
| Lazy retry resolver (cold-server recovery) | `internal/tools/mcp/lazy.go:60–182` |
| Tool result event emit | `internal/loop/loop.go:844` |
| Tool result content-block assembly | `internal/loop/loop.go:856` |
