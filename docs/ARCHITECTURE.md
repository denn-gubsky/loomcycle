# Architecture

This document describes the runtime end-to-end through v0.16.1. The MCP-integration story originally shipped in v0.4.0 (Streamable HTTP transport, SSE response decoding, startup-retry, sub-agent host-policy inheritance) was inverted in v0.8.15: loomcycle now ALSO exposes itself as an MCP server alongside being an MCP consumer. The `connector.Connector` Go interface unifies HTTP, gRPC, MCP, and future CLI wire transports around a single contract — HTTP server IMPLEMENTS, others CONSUME via direct method dispatch. v0.8.16 added the Interruption tool (human-in-the-loop primitive); v0.8.17 added Pause / Resume / Snapshot — runtime-wide quiesce + cross-version-portable JSON snapshot, the precondition for the multi-replica HA that later landed in v0.12.x. v0.8.18 promoted the v0.8.15 PREVIEW Connector methods to real impls (MCP tools become real for free; added `GetSnapshot`) and added the gRPC + Python adapter surfaces. v0.8.22 introduced the SkillDef substrate (versioned skills with active-pointer overlay parallel to AgentDef). v0.8.24 added the parity built-ins (`Grep`, `Glob`, `NotebookEdit`) so loomcycle agents have the same filesystem surface as Claude Code. v0.9.0 shipped Vector Memory (pgvector-backed semantic search on the existing `Memory` tool, gated by `LOOMCYCLE_PGVECTOR_ENABLED=1`) and per-agent `max_iterations` (yaml + AgentDef overlay). v0.9.1 made the resolved system prompt and the caller's initial user input the first two cards of every run transcript, so operators can audit "what the agent actually received" directly from the Web UI. Since v0.9.1 the runtime grew along eight more axes (each its own section below): the **LLM Gateway** + OpenAI-compatible chat/embeddings shims (v0.11.x), **OpenTelemetry** + per-tenant fairness (v0.10.x), **tool-use hooks**, **per-run named credentials**, **scheduled runs (ScheduleDef)**, and **multi-replica HA** (v0.12.x), **A2A interoperability** (v0.13.0), **input webhooks (WebhookDef)** (v0.14.x), **pluggable memory backends + a memory layer** (v0.15.0 / v0.16.0), and the **synthetic `code-js` provider** that runs operator JavaScript as a first-class agent (v0.16.0). The Connector interface is now 39 methods. For a higher-level pitch and quick-start, see the README. For the public roadmap, see `docs/PLAN.md`.

<p align="center">
  <img src="assets/architecture.png" alt="loomcycle architecture — app servers / CLIs / TS-Python SDKs / Claude Code / OpenAI-compat clients at the top; the single Go binary in the middle (wire surfaces incl. HTTP+SSE, gRPC, Web UI, MCP server, LLM Gateway, A2A server v0.13.0, input-webhook receiver v0.14.0 → middleware with per-tenant fairness → connector.Connector 39-method interface → agent loop → tool dispatcher with 19 agent-facing builtins + admin substrate Defs + MCP client + A2A client + sub-agent runner; run triggers from the ScheduleDef sweeper / webhooks / A2A; store with sessions/runs/events plus the substrate Def tables) → providers (six LLM endpoints + the synthetic in-process code-js provider) plus embedders, external MCP servers, a Mem9 memory backend, and remote A2A peers at the bottom" width="780" />
</p>

Diagram source: [`docs/architecture.d2`](architecture.d2) (regenerate with `d2 docs/architecture.d2 docs/assets/architecture.png`).

## Shape

`loomcycle` is a single Go binary (`bin/loomcycle` from `cmd/loomcycle/`) that:

1. Owns the LLM **tool-use loop** end-to-end (model → tool_use → tool_result → model). No vendor SDK in the loop, no bundled binary.
2. Talks to **providers** over their public HTTP APIs — Anthropic Messages, OpenAI Chat Completions, DeepSeek (OpenAI-compatible Chat Completions at a different base URL), Gemini Generative Language API, Ollama `/api/chat` (cloud + local) — plus the **synthetic `code-js` provider** (v0.16.0), a `Provider` like any other that runs operator JavaScript via goja **in-process, with no external call**.
3. Dispatches tool calls to **19 agent-facing built-in tools** (`Read`, `Write`, `Edit`, `Grep` (v0.8.24), `Glob` (v0.8.24), `NotebookEdit` (v0.8.24), `HTTP`, `WebFetch`, `WebSearch`, `Bash`, `Agent`, `Skill`, `Memory` (v0.8.0; +vector-search v0.9.0; +pluggable backend v0.15.0; +`add`/`recall` memory layer v0.16.0), `Channel` (v0.8.4), `AgentDef` (v0.8.5), `SkillDef` (v0.8.22), `Evaluation` (v0.8.5), `Interruption` (v0.8.16), `Context` (v0.8.7/8)) — plus the **admin substrate Def tools** (`MCPServerDef`, `ScheduleDef` (v0.12.7), `WebhookDef` (v0.14.0), `MemoryBackendDef` (v0.15.0), `A2AServerCardDef` / `A2AAgentDef` (v0.13.0)) — and routes to **MCP servers** (stdio + Streamable HTTP), **remote A2A peers** (`a2a__<peer>__<skill>`, v0.13.0), **LocalAPI gateways** (OpenAPI → tool-per-operation), or **sub-agents** (the `Agent` built-in).
4. Streams every event back to callers as **SSE** over a small HTTP API (`/v1/runs`, `/v1/sessions/{id}/messages`, `/v1/sessions/{id}/transcript`, `/v1/agents/{agent_id}`, `/v1/users/{user_id}/agents`, `/v1/agent_defs/*`, `/v1/skill_defs/*`, `/v1/evaluations/*`, `/v1/interrupts/*`, `/v1/hooks/*`, `/v1/_metrics/*`, `/v1/_pause`, `/v1/_resume`, `/v1/_state`, `/v1/_snapshots*`, `/healthz`).
5. **Exposes itself as an MCP server** via the `loomcycle mcp` subcommand (v0.8.15+) — 30+ meta-tools: run lifecycle (`spawn_run`, `cancel_run`, `get_run`, `list_runs`), agent registry (`register_agent`, `unregister_agent`, `list_agents`), the substrate builtins (`memory`, `channel`, `agentdef`, `skilldef`, `mcpserverdef`, `scheduledef`, `webhookdef`, `memorybackenddef`, `a2aservercarddef`, `a2aagentdef`, `evaluation`, `context`, `interruption_resolve`), Pause/Resume/Snapshot, and hook management. The `connector.Connector` interface is what every wire surface dispatches through.
6. **Quiesces on operator command** via `/v1/_pause` (v0.8.17) — idempotent tools cancel immediately, non-idempotent + external tools get a configurable grace window then force-cancel, new `/v1/runs` get 503; `/v1/_resume` releases the brakes; `/v1/_snapshots` captures running-state into a per-section-semver JSON envelope portable across loomcycle versions.
7. Persists sessions, runs, events (including the v0.9.1 `system_prompt` + `user_input` first-cycle events), agent_defs, skill_defs, evaluations, memory rows (with optional pgvector embeddings since v0.9.0), channel messages, process samples, dynamic_agents, interrupts, and snapshots to a pluggable `Store` (SQLite default; Postgres for HA, with pgvector when `LOOMCYCLE_PGVECTOR_ENABLED=1`). Runs carry a `pause_state` column (`running` / `pausing` / `paused`) so the resume sweep finds quiesced runs efficiently; runs also carry an optional `max_iterations` overlay (v0.9.0) sourced from yaml or the pinned AgentDef.
8. Caps concurrency with a **semaphore + bounded FIFO queue** to keep memory predictable on a small VPS.

Single-tenant out of the box; multi-tenant-shaped (every run carries `user_id` + optional `user_tier` + per-run named credentials; tracking + cancel APIs scope by user; per-tenant fairness on the concurrency layer shipped in v0.10.1 / cluster-wide v0.12.1; per-run credential substitution lets each agent authenticate to downstream MCP servers as the actual end-user). Today `user_id`/`tenant_id` are **caller-asserted** (attribution + fairness keys, not an authorization boundary) and one shared `LOOMCYCLE_AUTH_TOKEN` gates the HTTP frontier — correct for a single operator or a single trusted team. **Authority-derived multi-tenant authorization** (per-principal bearer tokens bound to a `(tenant, subject, scopes)` resolved from the token) is the v1.1.0 headline, RFC L.

## Repository layout (current — v0.16.1)

```
loomcycle/
├── cmd/loomcycle/                     binary entry-point (server / mcp / cli subcommands)
├── internal/
│   ├── agents/                        agent directory loader (yaml map + <name>.md overlay)
│   ├── api/
│   │   ├── http/                      HTTP+SSE server, auth, recovery, cancel routing (canonical Connector impl)
│   │   │                              + LLM Gateway + OpenAI-compat shims (v0.11.x)
│   │   ├── grpc/                      *loomgrpc.Server — proto handlers dispatching via Connector (v0.8.15+)
│   │   ├── a2a/                       A2A server — well-known card + REST/JSON-RPC/gRPC bindings (v0.13.0)
│   │   ├── webhook/                   input-webhook receiver — HMAC verify-before-parse (v0.14.0)
│   │   └── mcp/                       *lcmcp.Server — stdio + HTTP MCP server, meta-tools
│   ├── a2a/                           A2A executor + card signing + INPUT_REQUIRED↔Interruption (v0.13.0)
│   ├── auth/                          bearer-token middleware (constant-time compare)
│   ├── cancel/                        in-memory registry (agent_id → cancelFn) + cascade
│   ├── channels/                      persistent channel storage + notification bus (v0.8.4)
│   ├── cli/                           subcommands (pause/resume/snapshot/agent/run/operator-token)
│   ├── concurrency/                   semaphore + bounded FIFO queue + per-tenant fairness (v0.10.1)
│   ├── config/                        YAML + .env loader, agent/model/MCP/user-tier/schedule definitions
│   ├── connector/                     connector.Connector — 39-method Go interface (v0.8.15 → v0.16.x)
│   ├── coord/                         cluster backplane (Postgres LISTEN/NOTIFY) — cancel/pause/fanout/locks (v0.12.x)
│   ├── heartbeat/                     per-run last_heartbeat_at sweeper
│   ├── help/                          embedded help corpus for the Context tool (FS overlay via LOOMCYCLE_HELP_ROOT)
│   ├── hooks/                         operator hook substrate (register/list/delete + dispatch)
│   ├── loop/                          model→tool_use→tool_result iteration, RunOptions.MaxIterations
│   ├── memory/                        Backend interface + inprocess/mem9/fallback backends + add/recall
│   │                                  layer + hybrid ranker/dedup (v0.15.0 / v0.16.0)
│   ├── metrics/                       process-resource sampler + windowed handlers (v0.8.11)
│   ├── pause/                         Manager (RuntimeState, pauseCh, activeTools) — v0.8.17
│   ├── providers/
│   │   ├── anthropic/                 Messages API + native cache_control
│   │   ├── anthropic_oauth_dev/       reverse-engineered subscription OAuth (dev only, v0.11.10)
│   │   ├── openai/                    Chat Completions
│   │   ├── deepseek/                  Wraps openai/ with the DeepSeek base URL pre-baked
│   │   ├── gemini/                    Generative Language API + thinking_config (v0.7.x)
│   │   ├── ollama/                    /api/chat NDJSON (registered as both `ollama` cloud + `ollama-local`)
│   │   ├── codejs/                    synthetic code-js provider — operator JS via goja, replay model (v0.16.0)
│   │   ├── embedder/                  OpenAI + Gemini embedding drivers (v0.9.0)
│   │   ├── ratelimit/                 per-driver retry-after + backoff
│   │   └── provider.go                Provider interface (Call, Probe, ListModels) + Capabilities
│   ├── resolve/                       v0.7.0 model resolution matrix (tier + effort + availability)
│   ├── runner/                        loop driver shared by HTTP and MCP-streaming paths (+ PG session locks v0.12.x)
│   ├── runstate/                      in-process run-state bus (+ cluster fanout v0.12.x)
│   ├── scheduler/                     ScheduleDef sweeper — fires due rows in parallel (v0.12.7)
│   ├── skills/                        static skills (Approach A) + dynamic SkillDef substrate (v0.8.22)
│   ├── snapshot/                      Capture / Restore + per-section migration registry (v0.8.17)
│   ├── store/
│   │   ├── store.go                   Store interface (sessions / runs / events / + ~20 substrate tables)
│   │   ├── sqlite/                    modernc.org/sqlite (pure Go, no cgo) — 30+ migrations
│   │   └── postgres/                  pgx-based; pgvector-aware Memory store; cluster substrate (v0.12.x)
│   ├── tools/
│   │   ├── builtin/                   19 agent-facing builtins + 8 substrate Def tools (see Shape §3)
│   │   ├── a2a/                       A2A client — remote peers as a2a__<peer>__<skill> tools (v0.13.0)
│   │   ├── mcp/{stdio,http}/          MCP client transports (+ ${run.credentials} substitution)
│   │   ├── localapi/                  OpenAPI → tools
│   │   ├── policy/                    per-agent allow/deny + glob matching
│   │   └── tool.go                    Tool interface, Dispatcher, ctx-stash helpers
│   └── webui/                         embedded React Web UI served at /ui (assets bundled)
├── adapters/
│   ├── ts/                            @loomcycle/client (npm — v0.15.0)
│   └── python/                        loomcycle-py (v0.7.0; lags TS by a few releases)
└── docs/                              public docs (this file, TOOLS.md, PLAN.md, MCP_INTEGRATION.md, ...)
```

Internal planning notes (RFCs, decision history, ground-truth PLAN.md) live OUT of this repo at `~/work/loomcycle-internal/doc-internal/` — migrated from the in-repo `doc-internal/` folder in PR #100 (v0.8.15).

## Request flow (POST /v1/runs)

```
HTTP POST /v1/runs
  │
  ▼
authMiddleware                        → 401 on bad bearer (constant-time compare)
  │
  ▼
recoverMiddleware                     → panic → 500 JSON
  │
  ▼
config.ResolveAgentModel(agent)       → (provider, model)
  │
  ▼
providerResolver.Get(provider)
  │
  ▼
sem.Acquire(ctx)                      → 429 BackpressureError if queue full
  │
  ▼
filterTools(serverTools, agent.allowed, caller.allowed)
NarrowHosts(tools, caller.hosts, web_search_filter, callerAuthoritative)
                                      → per-run tool list with host policy baked in
  │
  ▼
openOrCreateSessionAndRun()           → SQLite rows (session, run); cancel registry entry
  │
  ▼
loopCtx = WithAgentTools + WithRunIdentity + WithHostPolicy
  │
  ▼
loop.Run(ctx, opts)
  │   for iter := 0..MaxIterations:
  │       provider.Call(ctx, req) → events
  │       collect text + tool_use blocks
  │       if !tool_use: break
  │       for each tool_use:
  │           dispatcher.Execute(ctx, name, input)  ── may spawn sub-agent
  │       append tool_result message
  │       (heartbeat update, cumulative usage)
  │
  ▼
emit events → SSE stream
finishRun(status, stop_reason, usage) → SQLite row update
sem.Release()
```

References: `internal/api/http/server.go` (request handling), `internal/loop/loop.go` (iteration), `internal/concurrency/semaphore.go` (acquire/release), `internal/cancel/registry.go` (cancel registration).

## Provider abstraction

```go
// internal/providers/provider.go
type Provider interface {
    ID() string
    Capabilities() Capabilities
    Call(ctx context.Context, req Request) (<-chan Event, error)
}

type Capabilities struct {
    NativePromptCache bool   // Anthropic cache_control
    ParallelToolCalls bool
    Streaming         bool
    MaxContextTokens  int
    SupportsThinking  bool
}

type Event struct {
    Type    EventType  // started | text | tool_call | tool_result | usage | done | error | retry
    // typed fields per variant
}
```

Each driver translates its provider's streaming shape into the same `Event` channel. The loop is provider-agnostic. Capability flags let the loop decline to set fields the provider doesn't honour (e.g. only Anthropic gets `cache_control` placement; Ollama tool-call IDs are synthesized by the loop).

Six driver registrations ship as of v0.8.3 (one package per provider, except `ollama` and `ollama-local` which share the `internal/providers/ollama/` package — same wire shape, different auth header + base URL):

| Driver       | API                       | Notes |
|---|---|---|
| `anthropic` | Messages (streaming SSE)   | Native `cache_control` on system blocks marked `cacheable: true`. `message_start.message.model` plumbed into final `Usage.Model` so callers get the resolved alias for pricing. |
| `openai`    | Chat Completions (streaming) | Index-keyed tool_call accumulator across deltas. Honours `[DONE]` sentinel. As of v0.6.0, captures the wire-resolved `model` field from each chunk envelope so `runs.model` populates correctly (also benefits any OpenAI-compatible endpoint — DeepSeek, vLLM). |
| `deepseek`  | Chat Completions (streaming) | Wraps the `openai` driver with `https://api.deepseek.com/v1` pre-baked and `ID()` returning `"deepseek"`. Same wire shape, retry strategy, and tool-call envelope. Operator opts in via `DEEPSEEK_API_KEY` env (optional `DEEPSEEK_BASE_URL` for self-hosted OpenAI-compatible mirrors). Distinct package so per-provider cost rollups don't conflate OpenAI and DeepSeek pricing. |
| `gemini`    | Generative Language API (streaming SSE) | v0.7.x driver. Reasoning-effort hint translated to Gemini's `thinking_config`. |
| `ollama`    | `/api/chat` (NDJSON), Bearer auth | **Hosted ollama.com.** Opts in via `OLLAMA_API_KEY` (optional `OLLAMA_CLOUD_BASE_URL` for vendor mirrors). Same `/api/chat` wire shape as local Ollama; Bearer header on every request. Treated as a paid-cloud provider in priority queues. Optional `LOOMCYCLE_OLLAMA_NUM_CTX` overrides the default per-request context window (see ollama-local row for rationale). |
| `ollama-local` | `/api/chat` (NDJSON), no auth | **Local-network Ollama.** Opts in via `OLLAMA_BASE_URL` (default `http://localhost:11434`; `disabled` opts out). No auth — local trust model. Tool-tuned models only (qwen3+, llama3.1+, mistral-large). Tool-call IDs synthesized as `lc-{iter}-{slot}` because Ollama doesn't issue them. Recent Ollama versions emit reasoning-model output in a separate `message.thinking` field which the driver currently drops — tracked as v0.7+ work. **`LOOMCYCLE_OLLAMA_LOCAL_NUM_CTX`** sets `options.num_ctx` on every request — required when prompts exceed Ollama's 4096-token server default, which otherwise truncates silently (no error, just a partial response that never reaches `end_turn`). |

Each driver has rate-limit retry logic (`internal/providers/ratelimit/`) — 429s and provider 5xx-with-retry-after preserve run context across the retry; observable as `event: retry` SSE frames.

**Cross-provider fallback invariants (v0.8.12+).** When the user_tier policy permits and a retryable error fires, `tryProviderFallback` in `internal/loop/loop.go` swaps to the next candidate from the tier's queue. Provider-specific transcript state that doesn't carry across families gets invalidated atomically alongside the switch via three typed events on the wire:

- **`EventProviderFallback`** (v0.8.2) — every successful switch emits this. Carries a structured `FallbackInfo` (failed_provider, failed_model, new_provider, new_model, attempt, user_tier, reason, cause_error).
- **`EventCacheInvalidated`** (v0.8.2) — emitted only when switching AWAY from Anthropic, because the `cache_control` breakpoints on system blocks are Anthropic-specific and don't transfer to other providers.
- **`EventReasoningInvalidated`** (v0.8.12) — emitted when the cross-provider strip pass cleared `Message.Reasoning` from one or more assistant turns in the in-flight conversation. `Message.Reasoning` (the single string field on `providers.Message`) carries no provider provenance; the OpenAI driver, which also backs DeepSeek, unconditionally echoes it back as `reasoning_content` on the wire. DeepSeek's API verifies the echo against what IT produced and 400s on mismatch (`"reasoning_content in the thinking mode must be passed back to the API"`). Cross-provider echoes always fail this check, so on every fallback the loop walks `messages` and zeroes the field on assistant turns where it was non-empty.

Same shape across all three: typed wire events, adapter-stable, cost retros should treat the run's downstream iterations on the new provider as both cache- and reasoning-cold.

As of v0.7.0 every driver also implements `Probe(ctx) error` and `ListModels(ctx) ([]string, error)` (used by the resolver — see below). Probe is a lightweight reachability + auth check; ListModels returns the wire aliases the provider currently serves. Both share the same round-trip in each driver's implementation.

References: `internal/providers/`, `internal/providers/anthropic/driver.go`, `internal/providers/ratelimit/`.

## Model resolution matrix

`internal/resolve/` (added v0.7.0) — the resolver picks `(provider, model)` for tier-using agents at request time. Inputs: agent yaml's `tier`, `effort`, optional per-agent `providers:` and `models:` overrides; library-wide `provider_priority` and `tiers` from the config. Output: a `Decision{Provider, Model, Effort}` the loop hands to the right driver.

State: `Availability` per provider, `ModelStatus` per model. Three orthogonal flags:
- `Excluded` — operator chose not to enable this provider (no API key, base URL unset). Set at startup; cleared by `SetReachable`.
- `Reachable` — most recent probe succeeded. Set/cleared by every probe sweep.
- per-model `Stalled` — runtime feedback from the loop on a 5xx-after-retry or in-stream error. Cleared by the next successful probe.

Lifecycle:
1. Startup: `cmd/loomcycle/main.go` builds the resolver, runs the first-round probe synchronously (parallel across providers, 5s deadline each), then starts a background goroutine that re-probes every `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS` (default 15 min, max 1 h).
2. Per-request: HTTP / gRPC server calls `Server.resolveAgent(name)` which routes pin-vs-tier and returns `(provider, model, effort)` for the loop.
3. On driver error: loop calls `MarkStalled(provider, model, reason)` (with `ctx.Err() == nil` guard so user cancels don't pollute the matrix).

Effort flow: agent yaml `effort: low|medium|high` → `Decision.Effort` → `RunOptions.Effort` → `providers.Request.Effort`. Each driver translates per its `Capabilities.SupportsEffort`:

| Driver | Translation |
|---|---|
| `anthropic` | `thinking.budget_tokens` (low → skip thinking; medium → 2048; high → 8192). Haiku always skips. Budget clamps to `max_tokens - 1024` if it would exceed `max_tokens`; drops below 1024 minimum. |
| `openai` | `reasoning_effort` (pass-through). API rejects on non-reasoning models with 400. |
| `deepseek` | Inherits OpenAI via the wrapper. |
| `ollama`, `ollama-local` | Both share the same driver: `SupportsEffort=false`. Loop logs once per Run when effort is dropped. |

References: `internal/resolve/matrix.go`, `internal/api/http/server.go` (`resolveAgent`, `markStalledFn`), `cmd/loomcycle/main.go` (`buildResolver`, `runResolveProbeOnce`, `runResolveProbeLoop`).

## Agent loop

`internal/loop/loop.go` — entry: `Run(ctx, RunOptions) → (RunResult, error)`.

Per iteration:
1. Send the cumulative message history to the provider.
2. Drain the event channel: emit text events to the consumer, accumulate tool_use blocks.
3. If no tool_use blocks were emitted (model said "done"), break.
4. For each tool_use block, call `dispatcher.Execute(ctx, name, input)` synchronously. Emit the tool_result as an event.
5. Append the assistant turn (with tool_use blocks) and the tool_result user turn to the history.
6. Tick the heartbeat (cheap UPDATE on the run row's `last_heartbeat_at`).

Iteration cap: `RunOptions.MaxIterations` (default 16). If the loop exhausts iterations while still mid-tool-use, `stop_reason` resolves to `max_iterations` (not `tool_use`) so callers can distinguish "model said it was done" from "we ran out of budget."

Token accounting: usage events are accumulated across iterations into `RunResult.Usage`; the final `event: done` frame carries the cumulative count.

References: `internal/loop/loop.go`, `internal/api/http/server.go makeRecordingEmit` (the SSE recorder).

## Tool dispatch

The `Dispatcher` (`internal/tools/tool.go`) maps tool name → `Tool.Execute(ctx, input)`. The set of tools registered on the dispatcher for a given run is the **intersection** of:

1. Operator's enabled built-ins + declared MCP tools + LocalAPI tools.
2. Agent's `allowed_tools` (YAML).
3. Caller's `allowed_tools` (request body).

Plus a per-run host narrowing (`HTTP`, `WebFetch`, `WebSearch`) via `builtin.NarrowHosts`, which replaces those tools in the per-run list with versions that have the caller's allowlist baked in.

The full policy story lives in `docs/TOOLS.md`. Architecturally:

```
serverTools  ──┐
agent.allow ──▶ filterTools  ──▶ NarrowHosts ──▶ Dispatcher
caller.allow ─┘                  (per-run     (per-run instance
                                  HTTP/WebFetch  passed to loop)
                                  /WebSearch)
```

References: `internal/tools/tool.go`, `internal/tools/policy/`, `internal/tools/builtin/narrowing.go`, `internal/api/http/server.go runRequest`.

### MCP HTTP transport (Streamable HTTP)

The HTTP MCP client (`internal/tools/mcp/http/client.go`) speaks Streamable HTTP per the MCP 2024-11-05 spec. Three behaviours worth knowing:

- **`Accept: application/json, text/event-stream`** on every outbound request. The official `@modelcontextprotocol/sdk` server-side transport returns 406 Not Acceptable if either media type is missing. Servers pick per-request whether to reply JSON (single-shot) or SSE (streaming), so the client must accept both.
- **SSE response decoding.** When the server replies with `Content-Type: text/event-stream`, the client extracts the JSON payload from the first complete SSE frame's `data:` lines (with multi-line `data:` joining via `\n`, CRLF tolerance, ignored `event:` / `id:` / `retry:` fields). Plain `application/json` responses are decoded directly. Both shapes are spec-compliant.
- **Per-run bearer substitution (v0.8.14+).** Operator yaml `mcp_servers.*.headers` can reference `${run.user_bearer}` and `${run.user_bearer:-FALLBACK}` (POSIX-style default). At outbound request-build time, `Client.do()` reads `tools.RunIdentity(ctx).UserBearer` and substitutes against a per-call local map copy — `c.headers` is never mutated, so the shared `Client` (one per server name, lives in `pool.go`'s registry) safely serves concurrent runs with distinct bearers. Missing bearer with no fallback drops the header and emits a once-per-call WARN log line; downstream MCP returns a clean 401. The substitution layer is fully decoupled from yaml-load-time `expandEnv` (`internal/config/config.go`) because that regex (`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`) structurally cannot match `${run.*}` — the `.` fails its `[A-Za-z0-9_]*` char class. This composition lets nested `${run.user_bearer:-${LOOMCYCLE_STATIC_BEARER}}` work during soak-phase rollouts: inner `${LOOMCYCLE_*}` resolves at yaml-load; outer `${run.user_bearer:-<resolved>}` flows to request-time.

### MCP startup retry

The `Pool.GetWithRetry` helper wraps `Pool.Get` with exponential backoff (500ms → 1s → 2s → 4s → 8s → 16s, cumulative ~32s). Used by `cmd/loomcycle/main.go` during the boot-time MCP-tool-discovery loop. Handles the chicken-and-egg start-order race: when an MCP server lives behind a peer that boots concurrently with loomcycle (e.g. a Next.js dev server compiling its `/api/mcp` route on first request), the first handshake attempt fails with `ECONNREFUSED` or a route-not-yet-compiled timeout. The shared 30s `mcpInitCtx` caps total retry across all servers; healthy servers handshake on attempt 1 (no backoff added). Retry attempts log so operators can see whether the wait is meaningful.

References: `internal/tools/mcp/http/client.go` (transport), `internal/tools/mcp/pool.go GetWithRetry`, `cmd/loomcycle/main.go` MCP-init loop.

## Sub-agents (the Agent tool)

The `Agent` built-in (`internal/tools/builtin/agent.go`) lets the model spawn a child run by name:

```json
{"name": "researcher", "prompt": "Investigate X and return JSON …"}
```

The Agent tool calls into the HTTP server's `runSubAgent` (registered automatically at `New()` time so it can close over the server's own `runSubAgent` method). The sub-run:

- Gets a fresh `agent_id` and a fresh session.
- Inherits the parent's `user_id` (from `tools.RunIdentity(ctx)`).
- Inherits the parent's caller-authoritative **host policy** (from `tools.HostPolicy(ctx)`) — so a parent that ran against `["localhost"]` spawns children that can also reach localhost. Without this, sub-agents fall back to the operator's static `LOOMCYCLE_HTTP_HOST_ALLOWLIST`, which usually excludes localhost callbacks.
- Records `parent_agent_id` on its run row, so the cancel registry can cascade.
- Runs the same loop, returns the child's final assistant text as the parent's tool_result.

Recursion is depth-capped at 16 by default (`MaxAgentDepth` on the `AgentTool`). Sub-failures are surfaced as `IsError: true` tool_results — the parent sees them, can retry / fall back / give up; loomcycle does NOT tear down the parent on a child error.

References: `internal/tools/builtin/agent.go` (the tool), `internal/api/http/server.go runSubAgent` (the runner), `internal/tools/tool.go` (`HostPolicy` / `RunIdentity` ctx helpers).

## Skills (Approach A + SkillDef substrate)

`internal/skills/` — at config-load, every directory under `LOOMCYCLE_SKILLS_ROOT` named `<skill>/SKILL.md` is read and parsed. Agents that list a skill in their YAML `skills: [voice-applier, position-relevance-filtering]` block get the skill's body **concatenated into their system prompt** — cacheable, baked into the agent's runtime view of the world. The skill's `allowed-tools` declared in its frontmatter must be a subset of the agent's `allowed_tools`; mismatches are rejected at config-load.

This is "Approach A" in the skills design — static bundling at config-load.

**SkillDef substrate (v0.8.22)** adds the dynamic counterpart: versioned skill definitions stored in `skill_defs` with active-pointer overlay (parallel to AgentDef). The model can author / fork / promote skills via the `SkillDef` built-in (`set`, `get`, `list`, `activate`, `fork`); the resolver overlay (`server.go resolveSkillBodiesForRun`) prefers an active DB row over the on-disk SKILL.md when one exists. The v0.9.1 `system_prompt` event carries a `skill_def_ids` map (skill name → resolved def_id) so operators can audit exactly which version of each skill the agent received.

References: `internal/skills/` (loader), `internal/tools/builtin/skill.go` (static-bundle dispatcher), `internal/tools/builtin/skilldef.go` (5-op dynamic tool), `internal/api/http/server.go resolveSkillBodiesForRun` (overlay resolution).

## LocalAPI gateway (scaffolded; not the v0.4 integration vehicle)

`internal/tools/localapi/` — operators register a local API in YAML by pointing at an OpenAPI spec:

```yaml
local_api:
  spec: openapi.yaml          # relative to this YAML's directory
  base_url: http://localhost:3000
  tool_name_prefix: jobs       # tools become jobs__<operationId>
```

At config-load, loomcycle parses the spec and registers one tool per operation, with input schemas derived from the OpenAPI parameters / request body schema. Tool names follow the configured prefix. The agent calls them like any other tool; loomcycle forwards the request to `base_url`.

**Status (v0.4.0):** Code, parser, dispatcher wiring, and unit tests are landed (`internal/tools/localapi/{loader,spec,tool,*_test}.go`). The runtime registers LocalAPI tools at startup when `cfg.LocalAPI.SpecPath` is non-empty. The first production consumer (jobs-search-agent) chose the MCP-server pattern instead — it runs its own `/api/mcp` Streamable-HTTP server exposing typed tools, which loomcycle consumes through the existing MCP HTTP transport. LocalAPI stays available for future consumers that prefer "wire an OpenAPI spec, get typed tools" without standing up an MCP server.

This is the alternative to running an HTTP MCP gateway in front of every internal API: same effect (one tool per endpoint), without the MCP server overhead. The trade-off vs. running an MCP server: LocalAPI has no per-call session, no `tools/list` dynamism, and no streaming responses — the OpenAPI spec is the contract.

References: `internal/tools/localapi/`, `cmd/loomcycle/main.go` (registration), `loomcycle.example.yaml` (commented `local_api:` section).

## Storage

`internal/store/{sqlite,postgres}/` — `Store` is a single Go interface with a SQLite-backed default (`modernc.org/sqlite`, pure-Go, no cgo) and a Postgres backend (`pgx`) for HA. Migrations live in numbered up/down SQL files (currently 17, `0001_init` → `0017_memory_embeddings`).

| Table | Added | Purpose |
|---|---|---|
| `sessions` | 0001 | One per logical session (a /v1/runs call or a continuation). |
| `runs` | 0001 | One per loop invocation. Columns include `status`, `started_at`, `completed_at`, `stop_reason`, token counts, `model`, `agent_id`, `parent_agent_id`, `last_heartbeat_at`, `error`, and (later additions) `user_tier`, `agent_def_id`, `pause_state`, `max_iterations`. |
| `events` | 0001 | Every SSE event the loop emitted — `seq` (autoinc PK), `session_id`, `run_id`, `ts`, `type`, `payload` (raw JSON BLOB). v0.9.1's `system_prompt` + `user_input` records ride this table verbatim. |
| `memory` | 0002 | Persistent `Memory` tool storage (agent + user scopes, atomic incr, TTL). |
| `user_tier` index | 0003 | Sparse indexing for per-tier rollups. |
| `channels` (+ `channel_messages`) | 0004–5 | Persistent inter-agent message bus + deferred-publish `visible_at` cursor. |
| `agent_defs` (+ `runs.agent_def_id`) | 0006–7 | Versioned AgentDef storage with lineage + per-run pin. |
| `evaluations` | 0008 | Versioned evaluation records (5-op AgentDef peer). |
| `process_samples` | 0009 | Built-in metrics sampler rows (v0.8.11). |
| `dynamic_agents` | 0010 | Agents registered at runtime via the MCP `register_agent` meta-tool. |
| `interrupts` | 0011 | Human-in-the-loop pending questions (v0.8.16). |
| `runs.pause_state` | 0012 | Per-run pause state with partial index on `('pausing','paused')`. |
| `snapshots` | 0013 | Captured runtime envelopes (v0.8.17). |
| `events` audit + by-run-seq indexes | 0014–15 | Operator audit + run-scoped transcript reads. |
| `skill_defs` | 0016 | Versioned skills substrate parallel to AgentDef (v0.8.22). |
| `memory_embeddings` | 0017 | Optional pgvector-backed semantic search rows (v0.9.0; Postgres-only, gated by `LOOMCYCLE_PGVECTOR_ENABLED=1`; SQLite refuses with `vector_unsupported`). |

Indexes: partial indexes on sparse columns (`agent_id`, `parent_agent_id`, `user_id`, `pause_state`) so cardinality stays low while sub-agent tracking + resume sweeps work at scale. Read replays for session continuation use `events_by_session(session_id, seq)`; per-run transcript reads use the v0.8.x by-run-seq index.

SQLite ships in WAL mode + `foreign_keys=ON` (single-writer is the SQLite trade-off). Postgres is the HA path — connection-pooled, multi-writer, and the only backend where Vector Memory works.

References: `internal/store/store.go` (interface), `internal/store/sqlite/` + `internal/store/postgres/` (backends), `internal/store/storetest/` (shared conformance suite).

## Concurrency

`internal/concurrency/semaphore.go` — counting semaphore with a bounded FIFO waiter queue.

- `MaxConcurrentRuns` slots active simultaneously.
- `MaxQueueDepth` waiters queue when slots are full.
- Past the queue depth, `Acquire()` returns `BackpressureError` → HTTP 429 with `code:"backpressure"`.
- `QueueTimeoutMS` per acquire.

Single global pool — no per-tenant fairness through v0.9.x. A noisy tenant can monopolise the pool. Per-tenant token-bucket on top is the obvious v0.9.x high-load step.

References: `internal/concurrency/semaphore.go`, YAML `concurrency:` block in `loomcycle.example.yaml`.

## Cancellation

`internal/cancel/registry.go` — in-memory map: `agent_id → cancel.Entry{RunID, SessionID, UserID, ParentAgentID, StartedAt, cancelFn}`.

- Populated on `runRequest` / `runSubAgent` entry; cleared via `defer` at run finish.
- `POST /v1/agents/{agent_id}/cancel` → `Cancel(agentID, reason)` → walks registry by `parent_agent_id` and fires every descendant's `cancelFn(ErrCancelledByAPI+reason)`.
- The loop respects `ctx.Done()`; provider drivers' streaming goroutines exit cleanly on ctx-cancel; `finishRun` checks `errors.Is(context.Cause(runCtx), ErrCancelledByAPI)` and writes `status=cancelled` (not `failed`).

Cascade only works for in-flight runs. Already-finished sub-agents stay finished; the cancel surface is "stop everything that's still running under this branch."

References: `internal/cancel/registry.go`, `internal/api/http/server.go cancelHandler`.

## Caching

Two layers in scope:

1. **Native provider cache** (Anthropic only). The loop carries a `Cacheable: true` flag on system content blocks. The Anthropic driver maps that to `cache_control: {"type":"ephemeral"}` on the wire. Agent prompts and skill bundles are typically cached this way; `cache_read_input_tokens` shows up in the Usage event.
2. **Response KV cache** — scaffolded (`internal/cache/`) but not wired in v0.4.0. v1.0 will land an in-memory LRU keyed on `(provider, normalized request)` to cut cost on idempotent reads.

References: `internal/providers/anthropic/driver.go` (cache_control placement), `internal/cache/` (stub).

## Observability (process sampler v0.8.11; OpenTelemetry v0.10.0)

Two independent observability surfaces: the always-available process-resource sampler (below) and opt-in OpenTelemetry tracing (end of section).

Built-in process-resource sampler. Periodic background goroutine; idle-gated on the concurrency semaphore so no work happens when no agents are running.

**What it captures (per tick):**

- `loomcycle_rss_bytes` — process RSS read from `/proc/self/status VmRSS` (Linux only; 0 on macOS/Windows)
- `loomcycle_heap_alloc_bytes` + `loomcycle_heap_inuse_bytes` — Go runtime heap from `runtime.ReadMemStats`
- `loomcycle_num_goroutines` — `runtime.NumGoroutine()`
- `loomcycle_cpu_pct_x100` — delta CPU% computed from `/proc/self/stat utime + stime` between successive ticks (Linux only; ×100 for integer storage, so 1250 means 12.5%)
- `active_runs` + `queued_runs` — from the concurrency semaphore at sample time
- Optional system-wide fields (when `LOOMCYCLE_METRICS_COLLECT_SYSTEM=1`): `system_cpu_pct_x100`, `system_mem_used_mb`, `system_mem_available_mb` read from `/proc/stat` + `/proc/meminfo`

**Persistence:**

- Single new table `process_samples` (sqlite + postgres). One row per tick. Time-indexed.
- Bounded retention: sweeper goroutine deletes rows older than `LOOMCYCLE_METRICS_RETENTION_DAYS` (default 7) at `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` cadence.
- Storage at defaults: ~30 MB/day, ~210 MB/week.

**Wire surface (bearer-authed):**

```
GET /v1/_metrics/samples?since=<RFC3339>&until=<RFC3339>&limit=N&cursor=<opaque>
    → { "samples": [ProcessSample,...], "next_cursor": "..." }

GET /v1/_metrics/runs/{run_id}
    → MetricsRunWindow JSON — peak / mean RSS + max CPU% for samples
      overlapping [started_at, COALESCE(completed_at, now)].
      Computed via SQL JOIN — no denormalized columns on `runs`.
      404 if run unknown; 200 with SampleCount=0 if no overlap.

GET /v1/_metrics/summary?period=1h|24h|7d
    → Aggregated buckets (mean/max RSS, p95 CPU%, max active_runs)
      computed in-Go from a single MetricsSampleWindow fetch.
```

All three endpoints return 503 with `enable_hint` when `LOOMCYCLE_METRICS_ENABLED=0` (the default — opt-in for v0.8.x; default-on planned for v0.9.x).

**Design decisions worth flagging:**

- **No denormalized `peak_rss` / `cpu_ms` columns on `runs`.** The `runs/{run_id}` endpoint computes via JOIN at API time. Avoids a hot-table UPDATE per sample tick AND avoids an unbounded in-memory `runID → peak` map in the sampler.
- **Samples carry only counts, not agent identity.** Operators correlate which agents were active via the time-window JOIN with the `runs` table; samples stay at ~250 bytes each.
- **Linux-only RSS/CPU** via build-tag-split (`proc_linux.go` / `proc_other.go`). Other platforms record the platform-independent fields with RSS/CPU as zero.
- **Soft-failure on `/proc` errors.** A hardened container that blocks `/proc/self/status` (gVisor, kata) gets logged ONCE at startup; subsequent ticks still write the row with the available fields.

References: `internal/metrics/sampler.go` (idle-gated ticker), `internal/metrics/proc_linux.go` (`/proc` readers), `internal/api/http/metrics_handlers.go` (three handlers), `internal/store/sqlite/sqlite.go` + `internal/store/postgres/postgres.go` (storage; the 4 `Metrics*` methods on the `Store` interface).

**OpenTelemetry tracing (v0.10.0, default OFF).** Distributed tracing via OTLP/HTTP, wired in `internal/otel/`. Activated only when `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` is set — empty endpoint installs a no-op tracer (`tracer.go`), so the cost is zero unless an operator opts in. The span tree mirrors the loop's shape: `loomcycle.run` (root) → `loomcycle.iteration` → `loomcycle.provider.call` + `loomcycle.tool.call` (+ `loomcycle.mcp.call` for MCP-routed tools). Spans carry attributes like `loomcycle.run_id`, `loomcycle.agent_id`, `loomcycle.model`, `loomcycle.input_tokens` / `loomcycle.cache_read_tokens`, and — for the synthetic provider — `loomcycle.provider.kind` + `loomcycle.provider.code_hash`. Config: `LOOMCYCLE_OTEL_SERVICE_NAME`, `LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS`, `LOOMCYCLE_OTEL_TRACES_SAMPLER_RATIO`. **Secret-exclusion is enforced at the recorder** (`internal/otel/recorder.go`): per-run credentials, peer bearers, and signing keys are never written to a span — the same boundary the credentials and A2A sections rely on.

## Connector abstraction + LoomCycle MCP (v0.8.15+)

<p align="center">
  <img src="assets/architecture-connector.png" alt="Connector abstraction diagram — the connector.Connector interface (39 methods at v0.16.x) in the centre; *lchttp.Server IMPLEMENTS it (canonical business logic); MCP, gRPC, and future CLI servers CONSUME via direct method dispatch; TS and Python adapters MIRROR the operation surface in their own languages over the HTTP wire" width="640" />
</p>

Diagram source: [`docs/architecture-connector.d2`](architecture-connector.d2) (regenerate with `d2 docs/architecture-connector.d2 docs/assets/architecture-connector.png`).

v0.8.15 introduced the `connector.Connector` Go interface (`internal/connector/connector.go`) — a 39-method contract (v0.16.x; grown from the original 20 by v0.8.16 `InterruptionResolve`, v0.8.18 `GetSnapshot`, v0.8.22 `SkillDef`, the three hook ops `RegisterHook` / `ListHooks` / `DeleteHook`, the channel-CRUD ops, and one op-discriminated method per substrate Def — `MCPServerDef` / `ScheduleDef` / `WebhookDef` / `MemoryBackendDef` / `A2AServerCardDef` / `A2AAgentDef`) that defines the operation surface every wire transport translates into. Architectural intent:

- `*lchttp.Server` **IMPLEMENTS** `connector.Connector` (`internal/api/http/connector_impl.go`, ~530 LOC of method bodies). This is the canonical business-logic surface.
- `*lcmcp.Server` (`internal/api/mcp/`) and `*loomgrpc.Server` (`internal/api/grpc/`) **CONSUME** the interface — they hold a `connector.Connector` field and dispatch each wire request through it. **No HTTP round-trips** — direct Go method calls.
- Future `*lccli.Server` (`loomcycle run --agent X "prompt"`) will follow the same pattern.
- TypeScript (`adapters/ts/`) and future Python adapters MIRROR the same operation surface in their own languages over the HTTP wire.

A compile-time interface assertion at `connector_impl.go:35` (`var _ connector.Connector = (*Server)(nil)`) means adding a method to `Connector` forces every implementation to update or the build breaks — drift is impossible.

### LoomCycle MCP server (stdio, v0.8.15)

`loomcycle mcp --config Y` starts BOTH the HTTP listener AND a stdio MCP listener on `os.Stdin` / `os.Stdout`. Operators wire it into MCP clients (Claude Code first) via `.mcp.json`:

```json
{"mcpServers": {"loomcycle": {"command": "/abs/path/to/loomcycle/loomcycle-mcp.sh"}}}
```

The companion `loomcycle-mcp.sh` wrapper at the repo root sources `.env.local` before exec — required because Claude Code spawns the binary with an empty env, missing the `LOOMCYCLE_*` + provider keys that upstream MCP server `${...}` placeholders expect.

**30+ tools exposed (v0.16.x):** run lifecycle (`spawn_run`, `cancel_run`, `get_run`, `list_runs`), agent management (`register_agent`, `unregister_agent`, `list_agents`), the substrate builtins (`memory`, `channel`, `agentdef`, `skilldef` (v0.8.22), `mcpserverdef`, `scheduledef` (v0.12.7), `webhookdef` (v0.14.0), `memorybackenddef` (v0.15.0), `a2aservercarddef` / `a2aagentdef` (v0.13.0), `evaluation`, `context`, `interruption_resolve` — the v0.8.16 bridge that lets external orchestrators be the answerer), Pause/Resume/Snapshot (9 tools: `pause_runtime`, `resume_runtime`, `get_runtime_state`, `create_snapshot`, `list_snapshots`, `get_snapshot` (added v0.8.18), `export_snapshot`, `restore_snapshot`, `delete_snapshot`), and hook management (`register_hook`, `list_hooks`, `delete_hook`). PREVIEW-mocked in v0.8.15; v0.8.17 shipped the real underlying primitives via HTTP+CLI+UI; v0.8.18 promoted the Connector layer from mocks to real delegation so MCP receives authoritative data. Wire shapes locked in v0.8.15 are unchanged across all three versions — orchestrators built against v0.8.15 keep working.

**Streaming via notifications:** when the client opts in through `initialize.capabilities.loomcycle.runEvents=true`, `spawn_run` drives `runner.RunOnce` directly and emits `notifications/loomcycle/run_event` per provider event before returning the final response. Without opt-in, blocking-only Connector path. Both produce identical final `SpawnRunResult` shape.

**Dynamic agents:** the new `dynamic_agents` table (SQLite + Postgres migration 0010) persists agents registered at runtime via `register_agent`. Periodic TTL sweeper (cadence: `LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS`, default 15 min) reclaims expired rows. Privileged builtins (Bash, Write, Edit) are stripped from `allowed_tools` unless `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1`. Static yaml-defined agents take precedence on name collision.

**Operator-policy ctx for MCP-direct builtin calls:** the underlying Memory / Channel / AgentDef / Evaluation / Context tools gate on per-agent policy values that don't exist for MCP-direct callers (no yaml agent definition behind a `tools/call`). `internal/api/mcp/context.go operatorCtx()` synthesises a permissive policy (all scopes allowed, `mcp-operator` synthetic agent name) before each builtin wrapper invocation. `TestOperatorCtx_AttachesAllRequiredPolicies` pins the contract — future tools growing policy gates force an update here.

**Sharp edges (still open):**
- Boot-time upstream MCP init can block stdio readiness for ~32 s if an upstream is misconfigured. The `.env.local`-sourcing wrapper mitigates; long-term, mcp-mode should make upstream init non-blocking.
- The HTTP listener binds 127.0.0.1:8787 alongside the stdio loop. Operators running the `loomcycle.sh` daemon cannot simultaneously run `loomcycle mcp` from the same machine.

References: `internal/connector/connector.go` (interface), `internal/api/http/connector_impl.go` (canonical implementation), `internal/api/mcp/server.go` (stdio I/O loop), `internal/api/mcp/handlers.go` (26 tool handlers), `internal/api/mcp/context.go` (`operatorCtx`), `loomcycle-mcp.sh` (env-loading wrapper), `cmd/loomcycle/main.go` (`mcp` subcommand dispatch).

## Pause / Resume / Snapshot (v0.8.17)

The runtime-wide quiesce primitive sits on top of three additions:

1. **`internal/pause/Manager`** — a per-process coordinator. Holds an atomic `RuntimeState` enum (`StateRunning` / `StatePausing` / `StatePaused`), a broadcast `pauseCh` (closed when pause is declared so blocked goroutines wake), and a `sync.Map` of `activeTools` (per-call cancel handles for every in-flight tool). The manager is always constructed in `cmd/loomcycle/main.go` and wired into `*http.Server` via `SetPauseManager`.
2. **Per-tool category dispatch.** `pause.ToolCtx(parent, toolID, toolName, input)` is the iteration-boundary hook called once per dispatched tool. Under `StateRunning` it returns the parent ctx unchanged + registers the entry. Under `StatePausing` / `StatePaused`, idempotent tools (`Read`, `WebFetch`, `WebSearch`, `Memory.get/list`, `Channel.peek`, `AgentDef.get/list`, `Context.*`, `Evaluation.get/aggregate`) get an immediately-cancelled ctx; non-idempotent + external (MCP) tools get a `WithTimeout(defaultTimeout)` ctx and the entry registers in `activeTools` so a deadline-fired drain can find and force-cancel them.
3. **Per-run `pause_state` column.** Additive migration `0012_runs_pause_state` on both SQLite + Postgres adds the column with a partial index on `('pausing', 'paused')` so the resume sweep is O(paused). Three values: `running` / `pausing` / `paused`.

The snapshot envelope is a JSON object with `schema_version` (envelope-level) + a `sections` map (`agent_defs`, `agent_def_active`, `memory`, `channels`, `evaluations`, `paused_runs`, optional `interaction_history`). Each section carries its own `version` (all at `"1.0"` today); `internal/snapshot/migrations` holds a per-section migration registry that walks chained migrators when a reader at a newer section version restores an older snapshot.

The Memory section's per-row schema includes an optional `embedding` field that is always null in v0.8.17. v0.9.x semantic memory populates it without bumping the section version — that's the additive-fields forward-compat rule, locked in the v0.8.17 RFC so the just-shipped snapshot format doesn't need a 1.0 → 1.1 migration when vector memory lands.

State transitions publish to `_system/runtime-state` (operator-declared channel; v0.8.6 system channels). No new SSE event types — the existing Channel pub/sub primitive carries the signal.

References: `internal/pause/manager.go` (Manager + ToolCtx), `internal/pause/tool_policy.go` (CategoryForInput), `internal/snapshot/snapshot.go` (Capture), `internal/snapshot/restore.go` (Restore with session-FK synthesis), `internal/snapshot/migrations/registry.go` (per-section migration), `internal/api/http/pause.go` (handlers), `internal/api/http/snapshots.go` (handlers), `internal/cli/pause.go` + `snapshot.go` (7 CLI subcommands), `web/src/components/PauseControls.tsx` + `web/src/pages/SnapshotsView.tsx` (operator UI).

## Vector Memory (v0.9.0)

The persistent `Memory` tool gained an optional semantic-search backend: `Memory.search` over agent or user-scoped rows by vector similarity rather than key lookup. The wire surface, scope model, and `(set/get/list/incr/delete)` ops are unchanged — `search` is purely additive.

- **Backend gating.** Postgres + pgvector only; opt-in via `LOOMCYCLE_PGVECTOR_ENABLED=1` (default off). SQLite refuses `search` calls with the structured error `vector_unsupported` so operators get a clear migration signal. `sqlite-vec` is deferred to v0.9.x.
- **Embedder substrate.** `internal/providers/embedder/` ships real OpenAI + Gemini drivers. Anthropic stubs out as `ErrEmbedderNotImplemented` (no public embedding API). Operator picks via `LOOMCYCLE_EMBEDDER_PROVIDER` + `LOOMCYCLE_EMBEDDER_MODEL`; the loop and tools are embedder-agnostic.
- **Schema.** Migration `0017_memory_embeddings` adds a side table keyed on `(scope, scope_id, key)` with a `vector` column (dimensionality fixed at embedder-load time). Writes are best-effort: a memory `set` always succeeds even if embedding generation fails — operators see the row with `embedding=null` and can trigger a backfill via `/v1/_memory/reembed`.
- **Snapshot forward-compat.** The v0.8.17 snapshot envelope's Memory section already reserves an `embedding` field (always null in v0.8.x). v0.9.0 populates it without bumping the section version — the additive-fields rule locked in the snapshot RFC means v0.8.x → v0.9.0 snapshots restore cleanly and forward.

References: `internal/store/postgres/memory_embeddings.go`, `internal/providers/embedder/`, `internal/api/http/memory_handlers.go` (admin endpoints: `embed_stats`, `reembed`).

## Per-agent max_iterations (v0.9.0)

Agents can override the loop's iteration cap via yaml frontmatter (`max_iterations: 32`) or via a versioned AgentDef overlay (`AgentDef.set max_iterations=32`). The value flows: agent definition → resolver → `loop.RunOptions.MaxIterations` → loop. Top-level + sub-agent runs both honour it; the MCP `register_agent` + `agentdef` meta-tools accept it; the TS + Python adapters and the Web UI surface it.

References: `internal/loop/loop.go RunOptions`, `internal/agents/agent.go MaxIterations`, `internal/api/http/server.go runRequest`, `internal/api/http/connector_impl.go RegisterAgent`.

## Transcript first-cycle visibility (v0.9.1)

Every run's event stream now starts with two events that describe **what the agent actually received**, before any model output:

- **`system_prompt`** — the fully-resolved system prompt (AgentDef body + skill bodies, after overlay + merge). Payload carries the resolved text plus provenance (`agent_def_id`, `skill_def_ids` map: skill name → active def_id). Emitted only when the agent has a non-empty system prompt.
- **`user_input`** — the caller's `Segments` from the original `POST /v1/runs` (or continuation `POST /v1/sessions/{id}/messages`). Already persisted since earlier versions; the v0.9.1 work made it actually render in the Web UI.

Both events sort first in the timeline by virtue of being emitted before the first model call. The Web UI renders them as two cards at the top of every run view (`AgentDetailPane.tsx`); the terminal transcript renderer (`TerminalTranscript`) renders them as plain text blocks. Adapters consume the events as flexible JSON; the TS adapter ships typed `UserInputPayload` + `SystemPromptPayload` interfaces.

The change is purely additive: existing transcript readers ignore unknown event types. Runs created before v0.9.1 won't have a `system_prompt` event; their Web UI view degrades gracefully (cards just don't appear).

References: `internal/api/http/server.go` (emission in `handleRuns`, `handleMessages`, `runSubAgent`), `internal/api/http/server.go resolveSkillBodiesForRun` (skill-provenance extension), `web/src/components/AgentDetailPane.tsx` (`case "system_prompt"` + `case "user_input"`), `web/src/api.ts` (typed payload interfaces).

## LLM Gateway + OpenAI-compat shims (v0.11.0→v0.11.4)

Three endpoints expose the resolver + provider-auth + retry stack as a direct LLM-call surface that **bypasses the agent loop** — no tool dispatch, no transcript persistence, and deliberately **no `runs`-table row per call** (gateway traffic is too high-cardinality, and the `events` table's NOT-NULL FK to `runs` is not worth faking):

- **`POST /v1/_llm/chat`** (v0.11.0) — native loomcycle wire shape. `handleLLMChat` parses, then defers to the shared `prepareGatewayDispatch`, which runs the four steps every gateway path needs: resolve `(provider, model, effort)` (explicit pin → tier → user-tier overlay), look up the provider, acquire a **per-user semaphore slot** (`sem.AcquireForUser`; empty `user_id` bypasses the per-user cap), and translate the wire request into a `providers.Request`. The provider is always called with `Stream=true`; the JSON path re-aggregates the channel, the SSE path proxies Anthropic-style `content_block_*` / `message_delta` / `done` frames.
- **`POST /v1/chat/completions`** (v0.11.3) — OpenAI Chat Completions shim. `handleOpenAICompatChat` translates the OpenAI request into the native `llmChatRequest`, delegates to the same `prepareGatewayDispatch`, then translates the response back into OpenAI chunk/completion shapes. The shim owns **zero substrate logic** — a routing/quota/retry bug shows up on `/v1/_llm/chat` too; a bug here is purely a wire-format translation bug.
- **`POST /v1/embeddings`** (v0.11.4) — OpenAI Embeddings shim. Calls `s.embedder.Embed()` (the same single instance the Memory tool uses); 503 `embedder_not_configured` when no embedder is set.

No cross-provider mid-call fallback (single call per request; same-provider rate-limit retry inside the driver still applies). Every request emits an always-on structured `llm_gateway:` audit log line (request_id, provider, model, tier, user_id, token counts, latency, status). All three are bearer-authed admin-scope endpoints.

References: `internal/api/http/llm_gateway.go` (`prepareGatewayDispatch`, `resolveGatewayRequest`), `internal/api/http/llm_gateway_translate.go`, `internal/api/http/openai_compat.go`, `internal/api/http/embeddings_compat.go`, `internal/api/http/server.go` (route registration).

## Tool-use hooks (v0.8.x→v0.12.5)

External apps register HTTP-webhook callbacks against an `(agents, tools, phase)` selector via `POST /v1/hooks` (`GET` to list, `DELETE /v1/hooks/{id}` to remove). The loop invokes them around tool dispatch in `dispatchOneTool` (`internal/loop/loop.go`) when a `hooks.Dispatcher` is wired; with no matching hook the path is a nil-slice fast return identical to the no-hook shape.

- **Pre-hooks** run before the dispatcher: a `PreHookResult` can rewrite the tool `Input`, short-circuit with a synthetic `Deny` result the model receives in lieu of running the tool, or supply `AllowHosts` to widen the per-call host policy — the last only when the hook's owner is listed in `hooks.permit_host_widen.owners` (or `LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS`); un-permitted entries are dropped with a WARN + metric.
- **Post-hooks** run after the dispatcher and can rewrite the `ToolResult` before the loop emits `EventToolResult`.
- **Trust invariant:** hooks run *after* the policy layer and may only narrow, never widen past the operator's static config (except the explicitly-gated `AllowHosts`). They cannot tear down a run; the worst case is one tool call short-circuited with a synthetic `IsError`.
- **Fail modes:** `FailOpen` (default — webhook timeout/5xx/network error passes the original input/result through, right for telemetry hooks) vs `FailClosed` (tool fails with `IsError`, right for security scanners).

Selector matching is exact-or-trailing-`*` prefix glob (no regex, no middle wildcards); `(Owner, Name)` is the identity so app restarts replace rather than duplicate. In cluster mode the `DBBackedRegistry` (v0.12.5, migration `0026_hooks`) persists registrations and invalidates peer caches over the `loomcycle.hook` backplane topic so a hook registered on one replica fires everywhere.

References: `internal/hooks/types.go`, `internal/hooks/dispatcher.go`, `internal/hooks/registry.go`, `internal/hooks/db_registry.go`, `internal/loop/loop.go` (`dispatchOneTool`), `internal/api/http/server.go` (`/v1/hooks` routes).

## Per-run named credentials (v0.12.7, RFC F)

A run request may carry `user_credentials: map<string,string>` — a named-credential bag generalising the v0.8.x single `user_bearer`. The map rides the run via `tools.RunIdentityValue.UserCredentials` (`internal/tools/tool.go`); for back-compat, `WithRunIdentity` promotes a bare `UserBearer` into `UserCredentials["default"]` when that key is unset (cloning the map, never mutating in place).

At the MCP transport boundary, operator yaml `mcp_servers.*.headers` can reference `${run.credentials.<name>}` (strict) or `${run.credentials.<name>:-FALLBACK}` (POSIX-style default). `substituteCredentialRefs` (`internal/tools/mcp/http/substitute.go`) resolves these per-request against a local map copy at outbound-build time — the shared `Client.headers` is never mutated, so concurrent runs send distinct credentials without coordination. A bare missing reference drops the header (clean downstream 401); a `:-` fallback fills it.

Sub-agents inherit the whole map identically (same trust posture as `UserBearer` — they act on behalf of the same end-user). Credentials are treated as secrets throughout: **never persisted to run transcripts, snapshots, or OTEL spans, and never logged in full**.

References: `internal/tools/tool.go` (`RunIdentityValue.UserCredentials`, `WithRunIdentity`), `internal/tools/mcp/http/substitute.go` (`substituteCredentialRefs`).

## Scheduled runs — ScheduleDef (v0.12.7, RFC E)

Operators declare run templates under the yaml `scheduled_runs:` map (`config.ScheduledRun`); an entry is either standalone (`schedule:` cron) or a per-user-tier template (`user_tier_schedules:`) that dynamic per-user forks specialise. The `internal/scheduler` package runs a sweeper goroutine (one per process; in cluster mode each replica runs its own, gated by a Postgres advisory lock so a row fires once cluster-wide). Each `tick` lists due rows and fires them **in parallel** up to `MaxConcurrentFires` (default `NumCPU()*4`), skipping entirely while the pause `Manager` is non-running.

The `ScheduleDef` built-in (`internal/tools/builtin/scheduledef.go`) is a 7-op tool — the five core ops `create` / `fork` / `get` / `list` / `retire` plus `add_hook` / `remove_hook` (each hook edit persists a new lineage version). Static yaml entries remain immutable ground truth; the tool authors *new* names only.

A fired schedule's `on_complete` hooks deliver results through one of three kinds (`internal/scheduler/dispatch.go`): `channel.publish`, `memory.set`, or `mcp.call`. Schedules resolve MCP-tool bearers via `user_credentials_from_env` against the operator's `EnvAllowlist` — the same RFC F credential mechanism, not a parallel one. Persisted in the `schedule_defs` table (migration `0029`).

References: `internal/scheduler/scheduler.go` (sweeper, `tick`, `fireOne`), `internal/scheduler/dispatch.go` (`on_complete` kinds), `internal/tools/builtin/scheduledef.go`, `internal/config/config.go` (`ScheduledRun`).

## Multi-replica HA (v0.12.0→v0.12.6)

Cluster mode activates when `LOOMCYCLE_REPLICA_ID` is set; Postgres is required (SQLite refuses to start in this mode). The coordination substrate is `internal/coord`, built on a single `Backplane` pub/sub abstraction implemented over **Postgres `LISTEN`/`NOTIFY`** (`PostgresBackplane`) — zero new infra dependency beyond the Postgres cluster mode already needs. Every topic is `loomcycle.`-prefixed to avoid collisions: `loomcycle.cancel`, `loomcycle.pause`, `loomcycle.runstate`, `loomcycle.channel`, `loomcycle.quota`, `loomcycle.hook`.

- **Cross-replica cancel** (Phase 3) — `CancelCoordinator` publishes `loomcycle.cancel`; a remote replica owning the run dispatches it to its local registry. When the owner replica's heartbeat is stale the cancel handler marks the run failed (`owner_replica_dead`) or returns `owner_replica_unreachable`.
- **Bus fanout** (Phase 4) — the in-process run-state `Bus` (`internal/runstate/bus.go`) and the Channel bus gain optional backplane fanout via `SetBackplane`, so an event published on one replica reaches subscribers on all.
- **Cluster-wide pause/resume** rides `loomcycle.pause`.
- **Singleton sweepers** (Phase 5) — `AdvisoryLock` wraps `pg_try_advisory_lock` so a sweep (replicas, schedules) runs on exactly one replica.
- **DB-backed session locks** (Phase 6) — `runner.PgSessionLocker` uses a session-scoped advisory lock keyed on `hash(session_id)`; the **DB-backed hook registry** (Phase 6) replaces the in-process one.

Tables: `replicas` (heartbeat table backing the cluster `/healthz` view, migration `0022`), `runs.replica_id` (`0023`), `user_quotas` (`0024`), `runtime_state` (`0025`).

References: `internal/coord/backplane.go`, `internal/coord/postgres_backplane.go`, `internal/coord/cancel_coordinator.go`, `internal/coord/advisory_lock.go`, `internal/coord/replica_store.go`, `internal/runner/session_locks_pg.go`, `internal/runstate/bus.go`.

## A2A interoperability (v0.13.0, RFC G)

loomcycle speaks the Agent-to-Agent protocol on both sides; off by default, gated by `LOOMCYCLE_A2A_ENABLED=1`.

**As an A2A server** (`internal/api/a2a/`): serves a well-known AgentCard at `/.well-known/agent-card.json` plus three protocol-binding mounts — REST (`/a2a/v1`, `TransportProtocolHTTPJSON`), JSON-RPC, and gRPC. REST + JSON-RPC are always served; the **gRPC binding is dropped under multi-tenant host/path routing** because the tenant cannot be derived from the gRPC transport (`Server.grpcEnabled`). A `principalInterceptor` authenticates every binding request uniformly. Cards are optionally signed (JWS over JCS-canonicalised JSON via `internal/a2a/sign`); card serving never 500s on a signing failure — it serves unsigned and traces.

**As an A2A client** (`internal/tools/a2a/`): remote peers become synthetic tools named `a2a__<peer>__<skill>` (mirroring `mcp__<server>__<tool>`), so an agent reaches a peer only by listing `a2a__<peer>__<skill>` (or an `a2a__<peer>__*` glob) in its allowlist.

Two substrate Defs let agents author these at runtime over operator-blessed names: **`A2AServerCardDef`** (the exposed card) and **`A2AAgentDef`** (remote-peer definitions), each a `create`/`fork`/`get`/`list`/`retire` tool (table `a2a_defs`, migration `0031`). A peer that returns `INPUT_REQUIRED` maps to loomcycle's Interruption primitive: the executor parks the run and a follow-up A2A message resumes it through the same `InterruptResolver` bus the HTTP resolve handler and Interruption tool use.

References: `internal/api/a2a/server.go`, `internal/api/a2a/card.go`, `internal/api/a2a/routing.go`, `internal/a2a/executor.go`, `internal/a2a/interruption.go`, `internal/a2a/sign`, `internal/tools/a2a/registry.go`, `internal/tools/builtin/a2aservercarddef.go`, `internal/tools/builtin/a2aagentdef.go`.

## Input webhooks — WebhookDef (v0.14.0/.1, RFC H)

External systems trigger work via signed `POST /v1/_webhooks/{name}` (`internal/api/webhook/`); off by default, gated by `LOOMCYCLE_WEBHOOKS_ENABLED=1`. A disabled Def is addressable but inert (404, indistinguishable from never-registered). The receiver does its own per-Def auth — it is NOT behind the global bearer middleware.

- **Verify-before-parse.** The raw body is read under the Def's size limit, then HMAC-verified over the *raw bytes* (never a re-serialized body) before any JSON parse. Three envelope shapes are supported: Stripe-style `<t>.<rawbody>`, GitHub-style `sha256=<hex>` over the raw body, and bare-hex (Linear and custom sources), all via constant-time `hmac.Equal`.
- **Strict JSONPath request→run mapping.** A `target → source-jsonpath` map projects request fields into the run; the JSONPath subset is validated up front (dot + array-index segments only — wildcards, filters, recursive descent are rejected).
- **Two-layer idempotency.** An in-memory dedup cache (Layer 1) plus a durable `runs.idempotency_key` unique-index check before spawn (Layer 2, migration `0033`); a replayed valid delivery returns a 200 idempotent ack.
- **Per-Def rate limit**, **per-run credentials** (RFC F: env-allowlisted `user_credentials_from_env` plus `user_credentials.<name>` payload-mapping overlay), and **`on_complete` hooks** (same `channel.publish`/`memory.set`/`mcp.call` kinds as ScheduleDef).

Delivery is one of two modes: `spawn` (build a `RunInput` and drive `runner.RunOnce`) or `channel` (relay the raw payload to a channel — the path a parked, long-poll-subscribed agent consumes to wake). The `WebhookDef` built-in is a `create`/`fork`/`get`/`list`/`retire` tool over operator-blessed names (table `webhook_defs`, migration `0032`).

References: `internal/api/webhook/server.go`, `internal/api/webhook/signature.go`, `internal/api/webhook/payload.go`, `internal/api/webhook/dedup.go`, `internal/api/webhook/runinput.go`, `internal/api/webhook/oncomplete.go`, `internal/tools/builtin/webhookdef.go`.

## Pluggable memory + memory layer (v0.15.0 RFC I, v0.16.0 RFC K)

The Memory tool no longer calls `store.Store` directly for its data ops — it routes through the `memory.Backend` interface (`internal/memory/backend.go`), six methods: `Get` / `Set` / `Delete` / `List` / `Search` / `Stats`. (`incr` and the reducer ops stay on the tool, calling the store directly — they're in-process atomic primitives, not part of the pluggable surface.) The default is the **in-process backend** (`backends/inprocess`, delegating to `store.Store` + the configured Embedder, vectors via sqlite-vec or pgvector), which is also the **unconditional fallback**.

- **`MemoryBackendDef` substrate** (RFC I MR-3a) — versioned backend definitions over operator-blessed `memory_backends.<name>:` names (5-op tool; table `memory_backend_defs`, migration `0034`). An agent's `memory_backend` field (yaml or AgentDef overlay) routes its Memory ops to a named backend.
- **Mem9 REST backend** (`backends/mem9`) — maps the six-op surface onto a Mem9-style `X-API-Key`-authed REST API. Tenancy is `scopedPrefix(scopeKey(...))`: `resolveTenancy` (`internal/tools/builtin/memory.go`) keys the tenant on `tools.RunIdentity(ctx).UserID` today (the field choke-point if a dedicated tenant field lands later — see RFC L). Per-run RFC F credentials resolve the API key. `fallback_on_error: inprocess` wraps the remote backend so a Mem9 outage degrades gracefully to the in-process store.
- **Hybrid ranker + search-time dedup** (`internal/memory/rank.go`, `dedup.go`) — `Search` over-fetches a cosine pool, applies the `RankConfig` hybrid score (`semantic + recency` decay; default `semantic=1` reproduces pure cosine), runs dedup *after* ranking and *before* the Top-K trim so the highest-ranked member of a duplicate cluster survives, then trims.

**MemoryLayer** (RFC K, `internal/memory/layer.go`) is an *optional, disjoint* capability for LLM-extract products: `Memory.add` ingests conversation messages (the backend may LLM-extract/reconcile durable facts, often asynchronously) and `Memory.recall` is a natural-language semantic search over server-assigned-ID facts — no key-addressed get/set. The tool probes the resolved backend's `Capabilities` once at op-routing time; the default in-process store does **not** implement MemoryLayer, so `execAdd`/`execRecall` **fail-close with `capability_unsupported`** (same posture as `vector_unsupported`), never a silent no-op. Because `fallback_on_error` reports the wrapper's capabilities (not the inner Mem9's), it and the memory layer are **mutually exclusive** — a backend wrapped for graceful KV degradation cannot also serve add/recall.

References: `internal/memory/backend.go`, `internal/memory/layer.go`, `internal/memory/rank.go`, `internal/memory/dedup.go`, `internal/memory/backends/{inprocess,mem9,fallback}/`, `internal/tools/builtin/memory.go` (`resolveTenancy`, `execAdd`, `execRecall`), `internal/tools/builtin/memorybackenddef.go`.

## Synthetic code-js provider (v0.16.0, RFC J)

`provider: code-js` runs operator-authored JavaScript (via the goja interpreter) instead of calling a model — a `providers.Provider` like any other (`internal/providers/codejs/`), so a code-agent shares the loop, OTEL spans, scheduler/webhook/A2A reachability, sub-agent composition, and evaluation surface; only the cost/determinism profile differs. The motivating use case is deterministic glue (ATS scrapes, known-shape SQL, format conversion, routing) at **zero token cost**. Off by default (`LOOMCYCLE_CODE_AGENTS_ENABLED=1`); the agent's JS lives at `agent_code/<name>/index.js`, and a broken/missing file fails **at startup** (`validateCodeAgents` in `cmd/loomcycle/main.go`), not at first fire.

- **Loop-driven dispatch.** code-js streams `EventToolCall` + `StopReason:"tool_use"` exactly as an LLM driver; the **loop** dispatches the tool (its ctx, hooks, OTEL, `${run.credentials}` substitution, WebFetch/HTTP host-allowlist) and re-invokes `Call` with the result. The provider never imports `internal/tools` — the one-way provider→loop→tools layering holds, so the OTEL/hooks/credential symmetry is real by construction. JS references each allowed tool by its **canonical name**: the three multi-op meta-tools (`Memory`/`Channel`/`Agent`) as objects with a method per op (`Memory.get(...)`), every other built-in (`WebFetch`, `Read`, …) and `mcp__<server>__<tool>` as a flat function.
- **Stateless replay execution.** Each `Call` builds a fresh goja runtime, fast-forwards the tool results already recorded in the transcript (the durable memoization log), and stops at the first un-recorded call (the "frontier") via `Interrupt`; the runtime is discarded within the `Call`. No continuation, no registry, no parked goroutine — so a run is **resumable across restart/replica**, cancel is just the `Call`'s ctx, and the "stateless across calls" Provider contract holds. Ambient non-determinism is hooked (`Math.random` seeded, `Date.now`/`new Date()` anchored to the run start) so replay is deterministic **by construction**; a control-flow divergence on replay fails loud (`code_agent_replay_divergence`).
- **Sandbox + bounds.** `eval` / `Function` deleted; no ambient fetch/fs/setTimeout. code-agents are **exempt from the loop's `MaxIterations` cap** (`Capabilities.UnboundedIterations`) — each turn is one internal tool-dispatch step, not a model turn — and bounded instead by a **whole-run wall-clock deadline** (`LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS`, derived from the stable run start). The run emits a `provider.code_hash` OTEL attribute (`provider.kind=synthetic-code`) for lineage.

References: `internal/providers/codejs/{provider,replay,bindings,sandbox,compiler,abi}.go`, `internal/providers/runmeta.go`, `internal/loop/loop.go` (RunMeta stamp + `UnboundedIterations`), `cmd/loomcycle/main.go` (`validateCodeAgents`).

## What's next (v1.0 → v1.1.0)

The v0.4.0-era deferred list is retired — OTEL, multi-replica HA, per-tenant fairness, the LLM gateway, A2A, webhooks, pluggable memory, and the synthetic code provider all shipped (above). What remains:

- **v1.0 — hardening + QA.** No new primitives: a security + robustness + runtime-QA pass across the v0.13–v0.16 surfaces, then the v1.0 tag. Authenticates with the single shared `LOOMCYCLE_AUTH_TOKEN` (correct for single-operator / single-trusted-team).
- **v1.1.0 — OSS multi-tenant authorization (RFC L, headline).** Per-principal bearer tokens (`OperatorTokenDef`), each bound to an authoritative `(tenant, subject, scopes)` resolved *from the token* — so per-subject fairness and per-tenant memory isolation become real (today they key on the caller-asserted `user_id`; the `resolveTenancy` choke-point above is where the authoritative tenant lands). Zero-disruption; enterprise-grade auth (SSO/RBAC/SCIM/signed audit) is a separate edition.
- **Beyond:** a settings UI, an operator cookbook of postures, broader distribution (Helm), Python adapter version parity (lags TS).

See `docs/PLAN.md` for the public roadmap.

## Verifying the runtime

```bash
go test ./...                                      # all green (run `make runtime-mock` for the live-binary suites; stress/soak are local-only)
go build -o bin/loomcycle ./cmd/loomcycle
./bin/loomcycle --config loomcycle.example.yaml
# in another terminal:
curl http://127.0.0.1:8787/healthz                 # {"ok":true}
```

For an end-to-end smoke against a real provider, set `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY`) and `LOOMCYCLE_AUTH_TOKEN` in `.env.local`, then POST to `/v1/runs` per the README quick-start.
