# Tools and tool policy

loomcycle exposes tools to agents through a **two-layer default-deny model**: every tool is disabled at the operator layer until env-configured, and every agent gets zero tools at the agent layer until `allowed_tools` is set in YAML. Both layers must say "yes" before a tool reaches the model.

This document explains the model end-to-end and what it means for operators wiring up new agents or new tools.

## The two layers

```
┌──────────────────────────────────────────────────────────────┐
│ Layer 1 — Operator (server-wide)                             │
│   Built-ins refuse every call when their env is unset.       │
│   MCP servers must be declared in mcp_servers in YAML, with  │
│   optional per-server allowed_tools narrowing.               │
└──────────────────────┬───────────────────────────────────────┘
                       │ (registered + enabled tools)
                       ▼
┌──────────────────────────────────────────────────────────────┐
│ Layer 2 — Agent (per-run)                                    │
│   Agent's allowed_tools in YAML lists the names exposed.     │
│   Empty / missing list → zero tools, full stop.              │
│   Caller's request body may further narrow (intersection).   │
└──────────────────────┬───────────────────────────────────────┘
                       │ (effective tool set)
                       ▼
                   Dispatcher
```

Both layers are intersection-only: nothing the model sees can ever exceed what the operator allowed at layer 1, and nothing exceeds what the agent definition allowed at layer 2.

## Layer 1 — operator-side: enabling tools

### Built-ins

Each built-in is registered into the dispatcher at process startup but **refuses every call** until its sandbox is configured via env. The model will see the tool's spec and may try to call it, but every call comes back with `{"is_error": true, "text": "<tool> is not configured ..."}`. This deliberate behaviour means the model gets a clear "not configured" signal it can self-correct from, instead of `unknown tool`.

| Tool        | Enabled by                                            |
|-------------|-------------------------------------------------------|
| `Read`      | `LOOMCYCLE_READ_ROOT=/path/to/sandbox`                |
| `Write`     | `LOOMCYCLE_WRITE_ROOT=/path/to/sandbox`               |
| `Edit`      | `LOOMCYCLE_WRITE_ROOT=/path/to/sandbox` (shared)      |
| `HTTP`      | `LOOMCYCLE_HTTP_HOST_ALLOWLIST=api.example.com,...`   |
| `WebFetch`  | (same allowlist as HTTP — shared backend)             |
| `WebSearch` | `BRAVE_API_KEY=...`                                   |
| `Bash`      | `LOOMCYCLE_BASH_ENABLED=1` + `LOOMCYCLE_BASH_CWD=...` |
| `Agent`     | Always registered (server-internal); per-agent `allowed_tools` controls who can spawn. |
| `Skill`     | `LOOMCYCLE_SKILLS_ROOT=/path/to/skills` (or skills inlined per-agent via YAML `skills:` list) |

Bash has additional warnings: it is **not a true sandbox** even when enabled. Run loomcycle inside a container or VM if Bash is exposed to untrusted prompts. See `internal/tools/builtin/bash.go` for the full warning.

Sandbox semantics (file tools):
- Paths must resolve **inside** the sandbox root after full `EvalSymlinks` evaluation. Symlinks pointing outside the root are refused.
- `Write` resolves the **parent** dir (target may not exist yet); `Read` and `Edit` resolve the target itself.
- All file writes are atomic via tempfile + same-directory rename.

SSRF semantics (network tools):
- `HostAllowlist` is **suffix-anchored** at a dot boundary: an entry `example.com` matches `example.com` and `api.example.com` but not `evilexample.com`.
- After DNS resolution, **private IPs are hard-blocked regardless of allowlist** — RFC1918, loopback, link-local (including the AWS/GCP metadata service at `169.254.169.254`), multicast, and IPv6 ULAs. This defeats DNS rebinding.
- Redirects re-validate the destination against the same allowlist on every hop.

### MCP servers

MCP servers are declared in YAML under `mcp_servers`. Each server's tools are registered as `mcp__{server}__{tool}` after the server's `tools/list` discovery completes.

Per-server narrowing at the operator layer:

```yaml
mcp_servers:
  brave-search:
    transport: stdio
    command: npx
    args: [-y, "@modelcontextprotocol/server-brave-search"]
    env: { BRAVE_API_KEY: "${BRAVE_API_KEY}" }
    # Operator-level filter: only these of the server's tools are
    # registered AT ALL. Others are invisible to every agent.
    allowed_tools: [brave_web_search]
```

Without `allowed_tools` on the server entry, every tool the server advertises is registered. With it, only the listed tools are registered, and the rest are dropped before any agent can ever request them.

## Layer 2 — agent-side: exposing tools

Each agent definition lists which tools it can access. The list is **anchored**: anything not on it is invisible.

```yaml
agents:
  cv-adapter:
    model: smart
    allowed_tools:
      - Read
      - Edit
      - "mcp__brave-search__*"   # glob — every brave-search tool that's registered
    system_prompt_file: ./prompts/cv-adapter.md

  ats-filter:
    model: local
    allowed_tools: [Read]         # only reads files
```

If you forget `allowed_tools`, the agent gets zero tools. The startup log will warn:

```
note: agent "myagent" has no allowed_tools — it will see zero tools (intentional default-deny; add allowed_tools to its YAML to expose tools)
```

This is a feature, not a bug. New agents start with no privileges; you grant them.

### Glob support

Entries ending in `*` match by prefix. The canonical use is exposing all tools from one MCP server: `"mcp__brave-search__*"`. Built-in names are short and unambiguous so globs aren't usually needed for them, but the same machinery applies.

### Caller-side narrowing

The HTTP request can carry `allowed_tools` in the body to further narrow the agent's set on a single run:

```http
POST /v1/runs
{
  "agent": "cv-adapter",
  "allowed_tools": ["Read"],         // narrowing — only Read for THIS run
  "segments": [...]
}
```

The effective set is the **intersection**: agent allowlist ∩ caller allowlist. The caller can shrink the agent's privileges for one run; it can never widen them.

Use cases:
- Multi-step pipelines where stage 1 only reads, stage 2 also writes.
- A web UI that exposes a "safe mode" that disables `Bash` even when the agent is normally allowed it.

## Per-request URL allowlist (HTTP / WebFetch / WebSearch)

The operator's `LOOMCYCLE_HTTP_HOST_ALLOWLIST` is a security floor — every host the agent might ever need to reach. For a single run, callers usually want to constrain further: this run is about scraping job listings from `linkedin.com` and `indeed.com`, that one is about reading from `bbc.co.uk`. Per-request narrowing exists for exactly this.

```http
POST /v1/runs
{
  "agent": "cv-adapter",
  "allowed_hosts": ["linkedin.com", "indeed.com"],
  "web_search_filter": "drop",       // optional; defaults to "drop"
  "segments": [...]
}
```

### Three-state `allowed_hosts`

| Field state           | Effective allowlist                                  |
|-----------------------|------------------------------------------------------|
| omitted (`null`)      | Operator's full static list (no narrowing)           |
| `[]` (empty array)    | **Deny all** — every HTTP / WebFetch call refuses    |
| `["a.com", "b.com"]`  | Operator list ∩ caller list                          |

Distinguishing `null` from `[]` matters: omitting the field means "I don't have an opinion, use the operator's list"; sending `[]` means "this run does no network calls".

### Intersection-only — caller can shrink, never widen

If the operator's static allowlist is `["api.linkedin.com"]` and the caller asks for `["evil.example", "api.linkedin.com"]`, the effective list is `["api.linkedin.com"]` — `evil.example` is silently dropped. The operator's policy is the floor; nothing the caller does can lift it. Suffix matching applies on both sides: `api.linkedin.com` is allowed under operator entry `linkedin.com`.

### Trust boundary

`allowed_hosts` MUST come from the trusted upstream caller, never from the model. The model can read its own `system` prompt and request body content but cannot construct a /v1/runs request. As long as the application calling loomcycle is the one supplying `allowed_hosts`, the threat model holds. **Don't pipe model-generated text into `allowed_hosts`** — that turns the policy into something the prompt can manipulate.

### `web_search_filter` for WebSearch

Brave Search returns whatever it found. With per-request narrowing in place, the model sees URLs it can't actually fetch, which wastes context tokens. Two options:

| `web_search_filter` | Brave behaviour preserved | Model sees results outside `allowed_hosts`? |
|---------------------|---------------------------|----------------------------------------------|
| `"drop"` (default when `allowed_hosts` is set) | Yes (Brave is paid) | No — non-matching results are filtered out, indices renumber |
| `"keep"`            | Yes                       | Yes — full result list passes through; caller filters downstream |

`drop` is the default because the typical agent is already context-constrained and showing it URLs it can't follow up on is wasteful. `keep` is for callers that want to see what Brave returned before applying their own narrowing logic (e.g. a UI that displays "we found N results on these other hosts too").

Filter mode is ignored when `allowed_hosts` is `null`.

### Session continuation

Continuations on `/v1/sessions/{id}/messages` re-supply `allowed_hosts` and `web_search_filter` per call. The list is **not** persisted on the session. This keeps "what hosts can this turn reach?" answerable from the request alone — no implicit state to chase. A continuation can change (typically narrow) the list as the conversation evolves.

### What this doesn't cover

- **Path-level filtering** (`example.com/api/v1/*` only) — out of scope; operators wanting that wire an HTTP MCP gateway in front.
- **Per-host method limits** (`linkedin.com` GET-only) — same reasoning.
- **Quota / rate limits per host** — orthogonal; belongs in a v0.4 hooks/observability story.

## What "default-deny" means in practice

| Situation                                                 | Result                                |
|-----------------------------------------------------------|---------------------------------------|
| Operator hasn't set `LOOMCYCLE_WRITE_ROOT`                | `Write`/`Edit` calls refuse with a clear message. |
| Agent omits `allowed_tools`                               | The model sees zero tools.            |
| Agent includes a tool the operator hasn't enabled         | The tool is registered (visible) but every call refuses. |
| Caller's request lists a tool the agent doesn't allow     | That tool is silently dropped from the run's set. |
| Agent globs `mcp__foo__*` but no such server is declared  | No tools — server isn't registered.   |

## Design rationale

**Why register tools the operator hasn't enabled?** So the model sees `{tool} is not configured` instead of `unknown tool`. The first is a signal the model can self-correct from ("oh, I shouldn't try that here"); the second is a confusing dead end.

**Why default-deny at the agent layer?** Adding a new built-in shouldn't silently grant every existing agent new privileges. With default-deny, new tools require explicit agent opt-in.

**Why allow caller narrowing but not widening?** The agent definition is the trust contract between operator and the runtime; the caller is upstream code that can reduce trust for one call but not raise it. This matches the principle that policy decisions flow inward (operator → agent → caller) and trust never compounds in the other direction.

## Testing the policy

Unit tests in `internal/tools/policy/policy_test.go` cover the matching rules (exact, glob, intersection).

Integration tests in `internal/api/http/server_test.go`:
- `TestAgentWithNoAllowedToolsSeesZeroTools` — empty allowlist → zero tools at the dispatcher.
- `TestAgentWithExplicitAllowSeeTool` — positive control: allow-listed tool reaches the model.

Empirical proof of the security invariant: inverting the default in `filterTools` (so empty allowlist exposes every tool) makes the first test fail. The test is what stops a future refactor from accidentally inverting the policy.

## The `Agent` tool — sub-agent spawning (v0.4.0)

The `Agent` built-in lets a parent agent spawn a child run by name:

```json
{"name": "researcher", "prompt": "Investigate X and return JSON …"}
```

**Auto-registration.** Unlike other built-ins, `Agent` is registered automatically by the HTTP server (it has to close over the server's own sub-run runner). It still respects per-agent `allowed_tools` — only agents that list `Agent` in their YAML can spawn children.

**What the child inherits from the parent (via ctx):**

- `user_id` — sub-agents bill to the same user.
- `parent_agent_id` — written to the child's run row; powers cascade-cancel.
- **Caller-authoritative host policy** (`allowed_hosts` + `web_search_filter`) — the parent's per-call host narrowing flows into the child. Without this propagation, sub-agents would silently fall back to the operator's static `LOOMCYCLE_HTTP_HOST_ALLOWLIST`, which usually excludes localhost callbacks. The fix landed in v0.4.0; see `internal/api/http/server.go runSubAgent` and `internal/tools/tool.go HostPolicy`.

**What the child does NOT inherit:**

- Tool allowlist — the child's `allowed_tools` is its YAML definition, narrowed only by the operator's enabled set. Parents cannot widen.
- Session — sub-agents get a fresh session.
- Tenant — inherited only for `user_id` purposes; multi-tenant isolation rules apply equally.

**Recursion cap.** `MaxAgentDepth = 16` by default. Deeper spawning attempts return `IsError: true` tool_results so the parent can decide to retry / fall back / give up.

**Failure mode is a tool error, not a run failure.** A sub-agent that crashes, times out, or rejects its input returns an `IsError: true` tool_result. Loomcycle does NOT tear down the parent on a child failure — the parent sees the error and decides what to do.

References: `internal/tools/builtin/agent.go`, `internal/api/http/server.go runSubAgent`, `internal/api/http/agent_subagent_test.go`.

## The `Skill` tool — Approach A (static bundling, shipped) and Approach B (dynamic, scaffolded)

**Approach A — static bundling (active in v0.4.0).**

At config-load, every directory under `LOOMCYCLE_SKILLS_ROOT` named `<skill>/SKILL.md` is read. Agents that list a skill in their YAML get the skill's body **concatenated into their system prompt** as a cacheable trusted-text block:

```yaml
agents:
  cv-adapter:
    model: smart
    allowed_tools: [Read, Write]
    skills: [voice-applier, cv-voice-applier]   # bodies merged into system prompt at config-load
    system_prompt_file: prompts/cv-adapter.md
```

Constraint: each skill's frontmatter declares its own `allowed-tools`; this list must be a **subset** of the agent's `allowed_tools`. Mismatches are rejected at config-load with a clear error. This prevents skills from advertising tools the agent can't actually invoke.

**Approach B — dynamic Skill tool (placeholder).**

The `Skill` built-in is registered when `LOOMCYCLE_SKILLS_ROOT` is set; the model can call it with `{"name": "voice-applier"}` to load a skill mid-conversation. In v0.4.0 the tool returns "unknown skill" — full Approach B implementation is v1.0 work. The hook is in place so prompts that reference the dynamic Skill tool can be authored today; they degrade gracefully (the tool reports the skill isn't available, the model continues without).

References: `internal/skills/`, `internal/tools/builtin/skill.go`.

## LocalAPI tools — OpenAPI gateway (scaffolded; not the v0.4 integration vehicle)

Operators register a local HTTP API by pointing at an OpenAPI spec in YAML:

```yaml
local_api:
  spec: openapi.yaml          # relative to this YAML's directory
  base_url: http://localhost:3000
  tool_name_prefix: jobs       # tools become jobs__<operationId>
```

At config-load, loomcycle parses the spec and registers one tool per operation. The configured prefix determines the tool names (e.g. `jobs__listProjects`, `jobs__createApplication`). Each tool's input schema is derived from the OpenAPI parameters + request body schema. Agents call them like any other tool; loomcycle forwards the request to `base_url`.

**Status (v0.4.0).** Code, parser, dispatcher wiring, and unit tests are landed (`internal/tools/localapi/`). The runtime registers LocalAPI tools at startup when `cfg.LocalAPI.SpecPath` is non-empty. The first production consumer (jobs-search-agent) chose the MCP-server pattern instead — it runs its own `/api/mcp` Streamable-HTTP server exposing typed tools (e.g., `mcp__jobs__getAgentContext`, `mcp__jobs__patchApplication`), which loomcycle consumes through the existing MCP HTTP transport. LocalAPI stays available for future consumers that prefer "wire an OpenAPI spec, get typed tools" without standing up an MCP server.

**Why this matters when it lands.** Today an agent prompt has to spell out `GET http://localhost:3000/api/agent/context` as a string the model writes. The model occasionally invents wrong hostnames or paths (the cv-batch-adapter cv-adapter children burned all their iterations guessing hostnames in May 2026). With LocalAPI: the model sees a typed tool `jobs__getAgentContext` with parameter docs, and the URL string is loomcycle's responsibility, not the model's.

**Allowlist behaviour.** LocalAPI tools are subject to the same default-deny: agents must list them in `allowed_tools` (glob `jobs__*` works). The HTTP destination (`base_url`) is NOT subject to `LOOMCYCLE_HTTP_HOST_ALLOWLIST` — operator opting into a `local_api` entry is the explicit grant.

**No SSRF defense for `base_url`.** LocalAPI is for trusted internal APIs only. If you need allowlisted external HTTP access from agents, use the `HTTP` / `WebFetch` built-ins, which carry the full SSRF protection.

References: `internal/tools/localapi/`, `cmd/loomcycle/main.go` (registration), `loomcycle.example.yaml` (commented `local_api:` example).

## Provider × tool matrix

What works with what (DeepSeek added in v0.6.0; behaviour identical to OpenAI for tool dispatch since it shares the driver):

|              | Anthropic | OpenAI | DeepSeek | Ollama (tool-tuned) |
|---|:---:|:---:|:---:|:---:|
| `Read` / `Write` / `Edit`  | ✅ | ✅ | ✅ | ✅ |
| `HTTP` / `WebFetch`        | ✅ | ✅ | ✅ | ✅ |
| `WebSearch`                | ✅ | ✅ | ✅ | ✅ |
| `Bash`                     | ✅ | ✅ | ✅ | ✅ |
| `Agent` (sub-agents)       | ✅ | ✅ | ✅ | ✅ |
| `Skill` (Approach A)       | ✅ | ✅ | ✅ | ✅ |
| `LocalAPI` (OpenAPI gateway) | ⏳ | ⏳ | ⏳ | ⏳ | (scaffolded — for future OpenAPI-without-MCP-server consumers) |
| MCP tools (stdio + HTTP)   | ✅ | ✅ | ✅ | ✅ |
| Native cache_control       | ✅ | ❌ | ❌ | ❌ |
| Parallel tool calls        | ✅ | ✅ | ✅ | depends on model |
| Streaming text + tool_use  | ✅ | ✅ | ✅ | ✅ |
| `Usage.Model` populated    | ✅ | ✅ (v0.6.0+) | ✅ | ✅ |

Ollama caveat: tool calling only works on **tool-tuned** models (qwen3+, Llama 3.1 instruct variants, Qwen2.5-Instruct, Mistral Nemo Instruct). Non-tool-tuned models will silently drop the `tools` field instead of calling them — Ollama's behaviour, not the driver's. Tool_use IDs are synthesized by the loop because Ollama doesn't issue them.

DeepSeek caveat: the v0.6.0 driver wraps the OpenAI driver, so it inherits OpenAI's behaviour exactly. The `deepseek-reasoner` model emits `reasoning_content` separately from `content`; the driver surfaces it as `EventDone.Reasoning` (since v0.7.0) and now also as live `EventThinking` events (since v0.7.x).

## Tool-use hooks (v0.7.x+)

Operator-supplied middleware around tool dispatch. External apps register HTTP-webhook callbacks against `(agent, tool, phase)` selectors; loomcycle invokes them around the dispatcher so the hook can rewrite the input, short-circuit with a synthetic result, or rewrite the post-tool result.

The canonical use case is **wrapping untrusted content** from `WebFetch` / `HTTP` / MCP results in trust-boundary markers so a downstream LLM treats payloads as data rather than instructions. Other shapes the seam supports: per-tool quotas, audit logs, content sanitisation, soft-deny patterns ("you tried to fetch X; here's a redacted version instead"), OTEL spans tied to tool invocations.

### Registration

```
POST /v1/hooks
{
  "owner": "jobs-search-web",       // app UID; (owner, name) is the identity
  "name":  "scan-webfetch",
  "phase": "post",                  // "pre" | "post"
  "agents": ["*"],                  // glob list; empty = ["*"]
  "tools":  ["WebFetch", "HTTP"],   // glob list; empty = ["*"]
  "callback_url": "https://jobs-search-web.local/api/hooks/scan",
  "fail_mode": "open",              // "open" (default) | "closed"
  "timeout_ms": 5000                // default 5000, ceiling 60000
}
→ 200 { "id": "hook_xxx" }

GET    /v1/hooks               // debug listing
DELETE /v1/hooks/{id}          // remove a registration
```

**Idempotency**: re-registering the same `(owner, name)` **replaces** the prior entry in-place (preserves chain ordering, mints a fresh ID). Solves the cascading-on-restart problem cleanly: an app that re-registers on its own startup never accumulates duplicates.

**No persistence** across loomcycle restart. Apps re-register on their own startup. If your app is down, your hooks aren't active — which matches reality (the app can't process callbacks anyway).

### Filtering

- `agents`: array of exact matches or `["*"]`. Empty / missing = `["*"]`.
- `tools`: array of exact matches; supports `prefix*` glob (`mcp__jobs__*`). Empty / missing = `["*"]`.
- A hook fires when **both** match. Multiple hooks can match the same call — they chain in registration order (Pre) or reverse order (Post, LIFO middleware).

### Webhook payload

**Pre** (`POST <callback_url>`):
```json
{
  "phase": "pre", "owner": "...", "hook_name": "...",
  "agent": "company-researcher", "user_id": "...", "agent_id": "...",
  "tool_call": { "id": "...", "name": "WebFetch", "input": { "url": "..." } }
}
→ { "input": {...} }                                 // rewrite
→ { "deny": { "is_error": false, "text": "..." } }   // short-circuit
→ {} or 204                                          // no change
```

**Post** (`POST <callback_url>`):
```json
{
  "phase": "post", "owner": "...", "hook_name": "...",
  "agent": "company-researcher", "user_id": "...", "agent_id": "...",
  "tool_call": { "id": "...", "name": "WebFetch", "input": {...} },
  "tool_result": { "is_error": false, "text": "...page body..." }
}
→ { "result": { "is_error": false, "text": "<untrusted>...</untrusted>" } }
→ {} or 204                                          // no change
```

### Composition

- **Pre chain**: registration order. The first non-nil `deny` short-circuits the rest of the Pre chain.
- **Post chain**: reverse registration order (LIFO middleware). Each hook sees the result the prior (inner) hook produced.
- Hooks within the chain run sequentially per-tool. The **parallel tool dispatcher** (v0.7.x) runs N tool calls in parallel; their hook chains fire in parallel too. The webhook server must handle concurrent calls.

### Failure modes

- **`fail_mode: "open"`** *(default)*: webhook timeout / 5xx / network error → original input or result passes through unchanged. Right for telemetry-shaped hooks where the hook should never block tool dispatch when the registering app is down.
- **`fail_mode: "closed"`**: webhook timeout / 5xx / network error → tool fails with `IsError: true`. Right for security-shaped hooks (injection scanners) where a down hook would let bypassed payloads through.

### Trust-boundary invariants

- Hooks run **after** the policy layer (`allowed_tools` / `allowed_hosts`). Hooks may narrow further or rewrite content — **never widen**.
- Hooks **cannot** tear down the agent run. The worst they can do is short-circuit one tool call with a synthetic `IsError: true` result.
- Webhook payloads include `agent_id` and `user_id` for correlation but **do NOT** include the agent's prompt or message history.

References: `internal/hooks/`, `internal/api/http/hooks.go` (registration routes), `internal/loop/loop.go::dispatchOneTool` (chain invocation).
