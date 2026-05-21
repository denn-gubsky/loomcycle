# Architecture

This document describes the runtime end-to-end through v0.9.1. The MCP-integration story originally shipped in v0.4.0 (Streamable HTTP transport, SSE response decoding, startup-retry, sub-agent host-policy inheritance) was inverted in v0.8.15: loomcycle now ALSO exposes itself as an MCP server alongside being an MCP consumer. The `connector.Connector` Go interface unifies HTTP, gRPC, MCP, and future CLI wire transports around a single contract — HTTP server IMPLEMENTS, others CONSUME via direct method dispatch. v0.8.16 added the Interruption tool (human-in-the-loop primitive); v0.8.17 added Pause / Resume / Snapshot — runtime-wide quiesce + cross-version-portable JSON snapshot, the precondition for v0.9.x multi-replica HA. v0.8.18 promoted the v0.8.15 PREVIEW Connector methods to real impls (MCP tools become real for free; added `GetSnapshot`) and added the gRPC + Python adapter surfaces. v0.8.22 introduced the SkillDef substrate (versioned skills with active-pointer overlay parallel to AgentDef). v0.8.24 added the parity built-ins (`Grep`, `Glob`, `NotebookEdit`) so loomcycle agents have the same filesystem surface as Claude Code. v0.9.0 shipped Vector Memory (pgvector-backed semantic search on the existing `Memory` tool, gated by `LOOMCYCLE_PGVECTOR_ENABLED=1`) and per-agent `max_iterations` (yaml + AgentDef overlay). v0.9.1 made the resolved system prompt and the caller's initial user input the first two cards of every run transcript, so operators can audit "what the agent actually received" directly from the Web UI. The Connector interface is now 26 methods. For a higher-level pitch and quick-start, see the README. For the public roadmap, see `docs/PLAN.md`.

<p align="center">
  <img src="assets/architecture.png" alt="loomcycle architecture — app servers / CLIs / TS-Python SDKs / Claude Code at the top, the single Go binary in the middle (wire surfaces incl. MCP server stdio+HTTP v0.8.15+ with 26 meta-tools → middleware → connector.Connector 26-method interface → agent loop → tool dispatcher with 19 builtins → store with sessions/runs/events plus agent_defs/skill_defs/evaluations/memory(+embeddings)/channels/interrupts/snapshots/process_samples/dynamic_agents), six LLM providers and external MCP servers at the bottom" width="780" />
</p>

Diagram source: [`docs/architecture.d2`](architecture.d2) (regenerate with `d2 docs/architecture.d2 docs/assets/architecture.png`).

## Shape

`loomcycle` is a single Go binary (`bin/loomcycle` from `cmd/loomcycle/`) that:

1. Owns the LLM **tool-use loop** end-to-end (model → tool_use → tool_result → model). No vendor SDK in the loop, no bundled binary.
2. Talks to **providers** over their public HTTP APIs — Anthropic Messages, OpenAI Chat Completions, DeepSeek (OpenAI-compatible Chat Completions at a different base URL), Gemini Generative Language API, Ollama `/api/chat` (cloud + local).
3. Dispatches tool calls to **19 built-in tools** (`Read`, `Write`, `Edit`, `Grep` (v0.8.24), `Glob` (v0.8.24), `NotebookEdit` (v0.8.24), `HTTP`, `WebFetch`, `WebSearch`, `Bash`, `Agent`, `Skill`, `Memory` (v0.8.0, + vector-search v0.9.0), `Channel` (v0.8.4), `AgentDef` (v0.8.5), `SkillDef` (v0.8.22), `Evaluation` (v0.8.5), `Interruption` (v0.8.16), `Context` (v0.8.7/8)), **MCP servers** (stdio + Streamable HTTP), **LocalAPI gateways** (OpenAPI → tool-per-operation), or **sub-agents** (the `Agent` built-in).
4. Streams every event back to callers as **SSE** over a small HTTP API (`/v1/runs`, `/v1/sessions/{id}/messages`, `/v1/sessions/{id}/transcript`, `/v1/agents/{agent_id}`, `/v1/users/{user_id}/agents`, `/v1/agent_defs/*`, `/v1/skill_defs/*`, `/v1/evaluations/*`, `/v1/interrupts/*`, `/v1/hooks/*`, `/v1/_metrics/*`, `/v1/_pause`, `/v1/_resume`, `/v1/_state`, `/v1/_snapshots*`, `/healthz`).
5. **Exposes itself as an MCP server** via the `loomcycle mcp` subcommand (v0.8.15+) — 26 meta-tools (`spawn_run`, `cancel_run`, `get_run`, `list_runs`, `register_agent`, `unregister_agent`, `list_agents`, the seven substrate builtins `memory` / `channel` / `agentdef` / `skilldef` / `evaluation` / `context` / `interruption_resolve`, `pause_runtime`, `resume_runtime`, `get_runtime_state`, `create_snapshot`, `list_snapshots`, `get_snapshot`, `export_snapshot`, `restore_snapshot`, `delete_snapshot`, `register_hook`, `list_hooks`, `delete_hook`). The `connector.Connector` interface is what every wire surface dispatches through.
6. **Quiesces on operator command** via `/v1/_pause` (v0.8.17) — idempotent tools cancel immediately, non-idempotent + external tools get a configurable grace window then force-cancel, new `/v1/runs` get 503; `/v1/_resume` releases the brakes; `/v1/_snapshots` captures running-state into a per-section-semver JSON envelope portable across loomcycle versions.
7. Persists sessions, runs, events (including the v0.9.1 `system_prompt` + `user_input` first-cycle events), agent_defs, skill_defs, evaluations, memory rows (with optional pgvector embeddings since v0.9.0), channel messages, process samples, dynamic_agents, interrupts, and snapshots to a pluggable `Store` (SQLite default; Postgres for HA, with pgvector when `LOOMCYCLE_PGVECTOR_ENABLED=1`). Runs carry a `pause_state` column (`running` / `pausing` / `paused`) so the resume sweep finds quiesced runs efficiently; runs also carry an optional `max_iterations` overlay (v0.9.0) sourced from yaml or the pinned AgentDef.
8. Caps concurrency with a **semaphore + bounded FIFO queue** to keep memory predictable on a small VPS.

Single-tenant out of the box; multi-tenant ready (every run carries `user_id` + optional `user_tier` + optional `user_bearer`; tracking + cancel APIs scope by user; per-run MCP bearer substitution lets each agent authenticate to downstream MCP servers as the actual end-user). Per-tenant fairness on the concurrency layer is v0.9.x work.

## Repository layout (current — v0.9.1)

```
loomcycle/
├── cmd/loomcycle/                     binary entry-point (server / mcp / cli subcommands)
├── internal/
│   ├── agents/                        agent directory loader (yaml map + <name>.md overlay)
│   ├── api/
│   │   ├── http/                      HTTP+SSE server, auth, recovery, cancel routing (canonical Connector impl)
│   │   ├── grpc/                      *loomgrpc.Server — proto handlers dispatching via Connector (v0.8.15+)
│   │   └── mcp/                       *lcmcp.Server — stdio + HTTP MCP server, 26 meta-tools
│   ├── auth/                          bearer-token middleware (constant-time compare)
│   ├── cancel/                        in-memory registry (agent_id → cancelFn) + cascade
│   ├── channels/                      persistent channel storage + notification bus (v0.8.4)
│   ├── cli/                           subcommands (pause/resume/snapshot/agent/run)
│   ├── concurrency/                   semaphore + bounded FIFO queue
│   ├── config/                        YAML + .env loader, agent/model/MCP/user-tier definitions
│   ├── connector/                     connector.Connector — 26-method Go interface (v0.8.15+)
│   ├── heartbeat/                     per-run last_heartbeat_at sweeper
│   ├── help/                          embedded help corpus for the Context tool (FS overlay via LOOMCYCLE_HELP_ROOT)
│   ├── hooks/                         operator hook substrate (register/list/delete + dispatch)
│   ├── loop/                          model→tool_use→tool_result iteration, RunOptions.MaxIterations
│   ├── metrics/                       process-resource sampler + windowed handlers (v0.8.11)
│   ├── pause/                         Manager (RuntimeState, pauseCh, activeTools) — v0.8.17
│   ├── providers/
│   │   ├── anthropic/                 Messages API + native cache_control
│   │   ├── openai/                    Chat Completions
│   │   ├── deepseek/                  Wraps openai/ with the DeepSeek base URL pre-baked
│   │   ├── gemini/                    Generative Language API + thinking_config (v0.7.x)
│   │   ├── ollama/                    /api/chat NDJSON (registered as both `ollama` cloud + `ollama-local`)
│   │   ├── embedder/                  OpenAI + Gemini embedding drivers (v0.9.0)
│   │   ├── ratelimit/                 per-driver retry-after + backoff
│   │   └── provider.go                Provider interface (Call, Probe, ListModels) + Capabilities
│   ├── resolve/                       v0.7.0 model resolution matrix (tier + effort + availability)
│   ├── runner/                        loop driver shared by HTTP and MCP-streaming paths
│   ├── skills/                        static skills (Approach A) + dynamic SkillDef substrate (v0.8.22)
│   ├── snapshot/                      Capture / Restore + per-section migration registry (v0.8.17)
│   ├── store/
│   │   ├── store.go                   Store interface (sessions / runs / events / + 13 substrate tables)
│   │   ├── sqlite/                    modernc.org/sqlite (pure Go, no cgo) — 17 migrations
│   │   └── postgres/                  pgx-based; pgvector-aware Memory store (v0.9.0)
│   ├── tools/
│   │   ├── builtin/                   19 builtins (see Shape §3)
│   │   ├── mcp/{stdio,http}/          MCP client transports
│   │   ├── localapi/                  OpenAPI → tools
│   │   ├── policy/                    per-agent allow/deny + glob matching
│   │   └── tool.go                    Tool interface, Dispatcher, ctx-stash helpers
│   └── webui/                         embedded React Web UI served at /ui (assets bundled)
├── adapters/
│   ├── ts/                            @loomcycle/client (npm — v0.9.1)
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

## Observability (v0.8.11+)

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

## Connector abstraction + LoomCycle MCP (v0.8.15+)

<p align="center">
  <img src="assets/architecture-connector.png" alt="Connector abstraction diagram — the connector.Connector interface (26 methods at v0.9.1) in the centre; *lchttp.Server IMPLEMENTS it (canonical business logic); MCP, gRPC, and future CLI servers CONSUME via direct method dispatch; TS and Python adapters MIRROR the operation surface in their own languages over the HTTP wire" width="640" />
</p>

Diagram source: [`docs/architecture-connector.d2`](architecture-connector.d2) (regenerate with `d2 docs/architecture-connector.d2 docs/assets/architecture-connector.png`).

v0.8.15 introduced the `connector.Connector` Go interface (`internal/connector/connector.go`) — a 26-method contract (at v0.9.1; grown from the original 20 by v0.8.16 `InterruptionResolve`, v0.8.18 `GetSnapshot`, v0.8.22 `SkillDef`, and the three hook ops `RegisterHook` / `ListHooks` / `DeleteHook`) that defines the operation surface every wire transport translates into. Architectural intent:

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

**26 tools exposed (at v0.9.1):** run lifecycle (`spawn_run`, `cancel_run`, `get_run`, `list_runs`), agent management (`register_agent`, `unregister_agent`, `list_agents`), the seven substrate builtins (`memory`, `channel`, `agentdef`, `skilldef` (v0.8.22), `evaluation`, `context`, `interruption_resolve` — the v0.8.16 bridge that lets external orchestrators be the answerer), Pause/Resume/Snapshot (9 tools: `pause_runtime`, `resume_runtime`, `get_runtime_state`, `create_snapshot`, `list_snapshots`, `get_snapshot` (added v0.8.18), `export_snapshot`, `restore_snapshot`, `delete_snapshot`), and hook management (`register_hook`, `list_hooks`, `delete_hook`). PREVIEW-mocked in v0.8.15; v0.8.17 shipped the real underlying primitives via HTTP+CLI+UI; v0.8.18 promoted the Connector layer from mocks to real delegation so MCP receives authoritative data. Wire shapes locked in v0.8.15 are unchanged across all three versions — orchestrators built against v0.8.15 keep working.

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

## What's deferred to v0.9.x → v1.0

The v0.4.0-era deferred list is no longer accurate. The current pipeline (see `docs/PLAN.md` for one-paragraph outlines):

- **n8n integration** (Phase 0+1) — first-class workflow runner so non-developers can compose loomcycle runs.
- **OTEL trace export** — OpenTelemetry traces over the loop iteration boundary; Prometheus metrics later.
- **Multi-agent fan-out** — formalised the implicit Agent-tool pattern that jobs-search-agent already exercises in production; needs a documented contract + concurrency-safe sub-run accounting.
- **Anthropic OAuth-dev** — operator OAuth flow for internal experimentation against Anthropic.
- **`loomcycle doctor`** — a one-shot diagnostics CLI for provider reachability, MCP server handshake, and config validation.
- **Per-tenant fairness** + **run-status memory cache** + **heartbeat sweeper hardening** — high-load capacity work, the v0.9.x track.
- **`sqlite-vec`** semantic-memory backend — local-friendly counterpart to v0.9.0's pgvector path.
- **Python adapter version parity** — currently lags TS by a few releases.

See `docs/PLAN.md` for the public roadmap.

## Verifying the runtime

```bash
go test ./...                                      # all green; current count ≈ 200 tests
go build -o bin/loomcycle ./cmd/loomcycle
./bin/loomcycle --config loomcycle.example.yaml
# in another terminal:
curl http://127.0.0.1:8787/healthz                 # {"ok":true}
```

For an end-to-end smoke against a real provider, set `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY`) and `LOOMCYCLE_AUTH_TOKEN` in `.env.local`, then POST to `/v1/runs` per the README quick-start.
