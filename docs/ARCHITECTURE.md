# Architecture

This document describes the v0.4.0 runtime end-to-end. v0.4.0 ships the MCP-integration story (Streamable HTTP transport, SSE response decoding, startup-retry, sub-agent host-policy inheritance) validated against jobs-search-agent as a real consumer; the LocalAPI gateway remains scaffolded as a convenience for OpenAPI-without-MCP-server cases but is no longer the integration vehicle. For a higher-level pitch and quick-start, see the README. For the public roadmap, see `docs/PLAN.md`.

## Shape

`loomcycle` is a single Go binary (`bin/loomcycle` from `cmd/loomcycle/`) that:

1. Owns the LLM **tool-use loop** end-to-end (model → tool_use → tool_result → model). No vendor SDK in the loop, no bundled binary.
2. Talks to **providers** over their public HTTP APIs — Anthropic Messages, OpenAI Chat Completions, DeepSeek (OpenAI-compatible Chat Completions at a different base URL), Ollama `/api/chat`.
3. Dispatches tool calls to **built-in tools**, **MCP servers** (stdio + HTTP), **LocalAPI gateways** (OpenAPI → tool-per-operation), or **sub-agents** (the `Agent` built-in).
4. Streams every event back to callers as **SSE** over a small HTTP API (`/v1/runs`, `/v1/sessions/{id}/messages`, `/v1/agents/{agent_id}`, `/v1/users/{user_id}/agents`, `/healthz`).
5. Persists sessions, runs, and events to a pluggable `Store` (SQLite default; Postgres + Redis adapters scaffolded for v1.0).
6. Caps concurrency with a **semaphore + bounded FIFO queue** to keep memory predictable on a small VPS.

Single-tenant out of the box; multi-tenant ready (every run carries `user_id`; tracking + cancel APIs scope by user). Per-tenant fairness is v1.0 work.

## Repository layout (as shipped in v0.4.0)

```
loomcycle/
├── cmd/loomcycle/                     binary entry-point
├── internal/
│   ├── api/http/                      HTTP+SSE server, auth, recovery, cancel routing
│   ├── cancel/                        in-memory registry (agent_id → cancelFn)
│   ├── concurrency/                   semaphore + bounded FIFO queue
│   ├── config/                        YAML + .env loader, agent/model/MCP definitions
│   ├── loop/                          model→tool_use→tool_result iteration
│   ├── providers/
│   │   ├── anthropic/                 Messages API + native cache_control
│   │   ├── openai/                    Chat Completions
│   │   ├── deepseek/                  Wraps openai/ with the DeepSeek base URL pre-baked
│   │   ├── ollama/                    /api/chat NDJSON (registered as both `ollama` cloud + `ollama-local`)
│   │   ├── ratelimit/                 per-driver retry-after + backoff
│   │   └── provider.go                Provider interface (Call, Probe, ListModels) + Capabilities
│   ├── resolve/                       v0.7.0 model resolution matrix (tier + effort + availability)
│   ├── skills/                        Approach A: static skill bundling at config-load
│   ├── store/
│   │   ├── store.go                   Store interface (sessions / runs / events)
│   │   └── sqlite/                    modernc.org/sqlite (pure Go, no cgo)
│   └── tools/
│       ├── builtin/                   Read, Write, Edit, HTTP, WebFetch, WebSearch, Bash, Agent, Skill
│       ├── mcp/{stdio,http}/          MCP transports
│       ├── localapi/                  OpenAPI → tools
│       ├── policy/                    per-agent allow/deny + glob matching
│       └── tool.go                    Tool interface, Dispatcher, ctx-stash helpers
├── adapters/ts/                       @loomcycle/client (npm)
├── docs/                              public docs (this file, TOOLS.md, PLAN.md)
└── doc-internal/                      internal planning notes (gitignored)
```

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
| `ollama`    | `/api/chat` (NDJSON), Bearer auth | **Hosted ollama.com.** Opts in via `OLLAMA_API_KEY` (optional `OLLAMA_CLOUD_BASE_URL` for vendor mirrors). Same `/api/chat` wire shape as local Ollama; Bearer header on every request. Treated as a paid-cloud provider in priority queues. |
| `ollama-local` | `/api/chat` (NDJSON), no auth | **Local-network Ollama.** Opts in via `OLLAMA_BASE_URL` (default `http://localhost:11434`; `disabled` opts out). No auth — local trust model. Tool-tuned models only (qwen3+, llama3.1+, mistral-large). Tool-call IDs synthesized as `lc-{iter}-{slot}` because Ollama doesn't issue them. Recent Ollama versions emit reasoning-model output in a separate `message.thinking` field which the driver currently drops — tracked as v0.7+ work. |

Each driver has rate-limit retry logic (`internal/providers/ratelimit/`) — 429s and provider 5xx-with-retry-after preserve run context across the retry; observable as `event: retry` SSE frames.

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

The HTTP MCP client (`internal/tools/mcp/http/client.go`) speaks Streamable HTTP per the MCP 2024-11-05 spec. Two behaviours worth knowing:

- **`Accept: application/json, text/event-stream`** on every outbound request. The official `@modelcontextprotocol/sdk` server-side transport returns 406 Not Acceptable if either media type is missing. Servers pick per-request whether to reply JSON (single-shot) or SSE (streaming), so the client must accept both.
- **SSE response decoding.** When the server replies with `Content-Type: text/event-stream`, the client extracts the JSON payload from the first complete SSE frame's `data:` lines (with multi-line `data:` joining via `\n`, CRLF tolerance, ignored `event:` / `id:` / `retry:` fields). Plain `application/json` responses are decoded directly. Both shapes are spec-compliant.

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

## Skills (Approach A)

`internal/skills/` — at config-load, every directory under `LOOMCYCLE_SKILLS_ROOT` named `<skill>/SKILL.md` is read and parsed. Agents that list a skill in their YAML `skills: [voice-applier, position-relevance-filtering]` block get the skill's body **concatenated into their system prompt** — cacheable, baked into the agent's runtime view of the world. The skill's `allowed-tools` declared in its frontmatter must be a subset of the agent's `allowed_tools`; mismatches are rejected at config-load.

This is "Approach A" in the skills design. Approach B (a dynamic `Skill` tool the model invokes at runtime to load skills it didn't statically know about) is scaffolded but not fully wired; the Skill tool returns "unknown skill" today. v1.0 work.

References: `internal/skills/`, `internal/tools/builtin/skill.go`.

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

`internal/store/sqlite/sqlite.go` — three tables, all keyed primarily by short hex IDs:

| Table       | Purpose | Notable fields |
|---|---|---|
| `sessions`  | One per logical session (a /v1/runs call or a continuation) | `id`, `tenant_id`, `agent`, `user_id` (v0.4+), `created_at` |
| `runs`      | One per loop invocation | `id`, `session_id`, `status`, `started_at`, `completed_at`, `stop_reason`, token counts (`input_tokens`, `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`), `model`, `agent_id` (v0.4+), `parent_agent_id` (v0.4+), `last_heartbeat_at`, `error` |
| `events`    | Every SSE event the loop emitted | `seq` (auto-increment PK), `session_id`, `run_id`, `ts`, `type`, `payload` (raw JSON BLOB) |

Indexes: partial indexes on the v0.4 sparse columns (`agent_id`, `parent_agent_id`, `user_id`) so cardinality stays low while sub-agent tracking works at scale. Read replays for session continuation use `events_by_session(session_id, seq)`.

WAL mode + `foreign_keys=ON`. Single-writer is the SQLite trade-off; Postgres adapter is v1.0 work.

References: `internal/store/store.go` (interface), `internal/store/sqlite/sqlite.go` (default backend).

## Concurrency

`internal/concurrency/semaphore.go` — counting semaphore with a bounded FIFO waiter queue.

- `MaxConcurrentRuns` slots active simultaneously.
- `MaxQueueDepth` waiters queue when slots are full.
- Past the queue depth, `Acquire()` returns `BackpressureError` → HTTP 429 with `code:"backpressure"`.
- `QueueTimeoutMS` per acquire.

Single global pool — no per-tenant fairness in v0.4.0. A noisy tenant can monopolise the pool. Per-tenant token-bucket on top is the obvious v1.0 step.

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

## What's deferred to v1.0

- **Memory tool** — agent-scoped persistent storage (the substrate for self-improving agents).
- **Channel tool** — persistent inter-agent message bus.
- **LoomHelp tool** — runtime introspection (the agent's view of its own toolset / config).
- **LoomCycle MCP** — loomcycle exposes itself as an MCP server for external orchestrators (Claude Code, etc.).
- **High-load runtime** — per-tenant fairness, Postgres `Store`, OTEL traces + Prometheus metrics, run-status memory cache, session-lock map GC, heartbeat sweeper.
- **Web monitoring frontend** — observability UI on top of the stored events.
- **Python adapter** — `pip install loomcycle`.

See `docs/PLAN.md` for the public roadmap with one-paragraph outlines per item.

## Verifying the runtime

```bash
go test ./...                                      # all green; current count ≈ 200 tests
go build -o bin/loomcycle ./cmd/loomcycle
./bin/loomcycle --config loomcycle.example.yaml
# in another terminal:
curl http://127.0.0.1:8787/healthz                 # {"ok":true}
```

For an end-to-end smoke against a real provider, set `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY`) and `LOOMCYCLE_AUTH_TOKEN` in `.env.local`, then POST to `/v1/runs` per the README quick-start.
