# Roadmap

This is the public roadmap. For decision history, regret notes, and per-version commit-by-commit details, see `doc-internal/PLAN.md` (gitignored).

## v0.7.0 — current

**Status: shipped (2026-05-08).** Tag `v0.7.0` on `main`, merged via PRs #21 + #22 + #23. Adds the model-resolution matrix: agents declare a tier (low / middle / high) plus an optional effort hint, the runtime picks (provider, model) against an availability matrix that's live-probed at startup and re-probed every 15 minutes. Closes the v0.7+ near-term scope from v0.6.0.

**What's in v0.7.0 (vs v0.6.0):**

- **Tier-based resolution.** Agent yaml declares `tier: low | middle | high`; resolver walks library priority + tier candidates and picks the first available `(provider, model)`. Per-agent overrides for `providers:` (full priority replacement) and `models:` (full tier-candidate replacement) cover the asymmetric pinning cases — see the cv-generator / ai-detector example in `loomcycle.example.yaml`. Explicit `provider:+model:` pins from v0.6.x continue to work unchanged.
- **Live probes per provider.** `internal/providers.Provider` gains `Probe(ctx) error` and `ListModels(ctx) ([]string, error)`. Each driver implements its variant: Anthropic / OpenAI / DeepSeek hit `/v1/models`, Ollama hits `/api/tags`. Startup runs all configured probes in parallel with a 5-second deadline; results seed the matrix.
- **`Excluded` flag for unconfigured providers.** Providers without API keys (or for Ollama, no base URL) are explicitly marked `Excluded` in the matrix — distinct from "probe attempted but failed". `Snapshot()` surfaces both flags so dashboards can render "deliberately not enabled" apart from "tried and failed". Operators see startup logs like `resolve probe: deepseek excluded (DEEPSEEK_API_KEY not set)`.
- **Reactive stall feedback.** Loop calls `MarkStalled(provider, model, reason)` on driver errors that suggest the model is broken (5xx after retry, mid-stream errors). `ctx.Err()` guards prevent user-cancellations from polluting the matrix. Next periodic probe revives or confirms the stall. Stall is per-`(provider, model)` so one bad model doesn't take down a whole driver.
- **Per-driver effort translation.** Agent yaml declares `effort: low | medium | high` alongside the tier. Drivers translate where supported: Anthropic → `thinking.budget_tokens` (low → skip; medium → 2048; high → 8192; haiku always skips); OpenAI → `reasoning_effort` (pass-through verbatim); DeepSeek inherits OpenAI; Ollama is a no-op. The loop logs `effort dropped` once per Run when an agent declared effort but landed on a `SupportsEffort=false` provider.
- **Periodic re-probe.** Background goroutine on the configured cadence (default 15 min, max 1 hour via `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS`). Tied to `bgCtx` for graceful shutdown alongside the heartbeat sweeper and session-lock GC.
- **Constant-time bearer compare hardening (carry-over from v0.6.0).** `internal/auth.CompareBearer` (sha256+CTC) replaces raw `subtle.ConstantTimeCompare` on both HTTP and gRPC. Closes the length-leak side channel.

**Architecture decisions:**

- **Anthropic budget clamping.** Anthropic's API requires `thinking.budget_tokens < max_tokens` AND `≥ 1024`. When the requested budget would equal or exceed `max_tokens`, the driver clamps to `max_tokens - 1024` (leaves 1024 for the response). Below the 1024 minimum, the field is dropped entirely. Operators wanting `tier: high + effort: high` to actually get the full 8192 thinking budget should set `max_tokens: 16384` explicitly.
- **No live cutover for stall** — operators see `runs.model` reflecting the wire-resolved alias from PR 2 onwards, so cost retros remain accurate even when the resolver fell through to a different candidate mid-Run.
- **No `runs.tier` column** — the resolver's input (the requested tier) and its output (resolved provider+model) are both observable today. Adding a column for "what tier did this come from" was deferred until a consumer asks for it.

For the v0.6.x cost-routing strategy that drove this work, see [v0.6.0](#v060--earlier).

## v0.6.0 — previous

**Status: shipped (2026-05-08).** Tag `v0.6.0` on `main`, merged via PRs #18 + #19 + the OpenAI driver `Usage.Model` fix. Provider matrix now covers four backends: Anthropic for user-sensitive paths, DeepSeek for high-volume public-data work, Ollama (local llama) for offline / cost-floor scenarios, OpenAI for general use. Per-agent provider routing in YAML lets a consumer mix and match by data sensitivity.

**What's in v0.6.0 (vs v0.5.5):**

- **DeepSeek provider** (`internal/providers/deepseek/`) — thin wrapper around the existing OpenAI driver with the DeepSeek base URL pre-baked and `ID()` returning `"deepseek"`. DeepSeek's Chat Completions endpoint is OpenAI-compatible, so the wire shape, SSE framing, retry strategy, and tool-call envelope all reuse the OpenAI plumbing unchanged. Operator opts in via `DEEPSEEK_API_KEY` env; `DEEPSEEK_BASE_URL` overrides for self-hosted OpenAI-compatible mirrors (e.g. vLLM serving a DeepSeek model). Per-agent yaml: `provider: deepseek`.
- **OpenAI driver: `Usage.Model` populated from chunk envelope.** Pre-fix, `streamEvents` extracted token counts from the streamed `usage` chunk but never captured the model field, so `runs.model` was always empty for any OpenAI-compatible endpoint (OpenAI itself, DeepSeek, vLLM, ...). Same regression class as the v0.4.0 anthropic fix; latent until the DeepSeek live test surfaced it. Now every OpenAI-compatible run records the wire-resolved alias (e.g. `deepseek-chat-v3-0324`, `gpt-4o-mini-2024-07-18`) — what the provider actually billed against, which matters for cost retros.
- **Ollama live integration tests** (`internal/providers/ollama/live_test.go`) — three tests (probe, chat, tool call) gated by `OLLAMA_TEST_BASE_URL`. v0.4.0 shipped the Ollama driver with httptest fakes only; the live coverage validates qwen3:14b on RTX 5080 (16GB VRAM) end-to-end as the offline / cost-floor backend.
- **`internal/auth.CompareBearer`** — sha256+CTC helper used by both HTTP and gRPC bearer-token middleware. Plain `subtle.ConstantTimeCompare` returns 0 immediately on length mismatch, leaking the expected token's length via timing; hashing both sides to a fixed-length digest before the constant-time compare closes that channel. Strengthens auth on both wire surfaces.

**Provider routing intent (jobs-search-agent first):**

| Sensitivity | Backend | Rationale |
|---|---|---|
| User-sensitive (CV / CL generation, profile-aware q&a) | **Anthropic** | Best quality + privacy posture for user data |
| Public data (job scoring, ATS filtering, company profiling, position relevance) | **DeepSeek** | Order-of-magnitude cheaper than Anthropic for high-volume work |
| Offline / private / cost floor | **Ollama** (local llama) | qwen3:14b on RTX-class VRAM gives DeepSeek-comparable quality at near-zero marginal cost |
| General / new agents during development | **OpenAI** | Standard choice for prototyping; pin to a specific model to avoid drift |

Architecture decision: DeepSeek is a separate `provider: deepseek` rather than `provider: openai` with a quirky base URL. Three reasons: (1) explicit yaml config documents intent, (2) per-provider `runs.model` rollups can't conflate OpenAI and DeepSeek pricing, (3) a place to absorb DeepSeek-specific quirks (reasoning_content for the reasoner model, future rate-limit header differences) without polluting the OpenAI driver.

## v0.5.5 — earlier

**Status: shipped (2026-05-08).** Tag `v0.5.5` on `main`, merged via PR #16. Wire surface coverage: gRPC alongside HTTP+SSE, async Python adapter as a first-class consumer.

**What's in v0.5.5 (vs v0.5.0):**

- **gRPC server** (opt-in via `LOOMCYCLE_GRPC_ADDR`). All seven RPCs mirror the HTTP+SSE surface 1:1 — `Run`, `Continue`, `GetAgent`, `CancelAgent`, `ListUserAgents`, `GetTranscript`, `Health`. Both wires share the same store, cancel registry, runner, and concurrency semaphore — picking gRPC vs. HTTP is a wire-format decision, not a feature decision. A cancel issued via gRPC reaches a run started via HTTP and vice versa.
- **`internal/runner/`** wire-agnostic seam. The HTTP server satisfies `runner.Runner`; the gRPC server delegates to the same instance. Compile-time guard `var _ runner.Runner = (*Server)(nil)` keeps the interface conformance honest.
- **Python adapter** (`adapters/python/`, `pip install loomcycle`) — async `LoomcycleClient` over `grpc.aio` covering all seven RPCs. Frozen-dataclass events (`AgentEvent`, `ToolUse`, `Usage`, `Retry`); typed exceptions over `grpc.StatusCode` codes; PEP-561 `py.typed` marker. Promotes the Python adapter from "deferred" to a shipped consumer.
- **Synthetic registration frames.** `Run`/`Continue` server-stream emit a wire-stable `session` + `agent` frame pair before any provider event so adapters can capture `(agent_id, run_id, session_id, parent_agent_id)` without re-decoding the loop's transcript. The Python adapter swallows these into a `RunHandle`; the HTTP+SSE side-channel exposes the same data via existing event types.
- **Operator guide:** [GRPC.md](GRPC.md) covers enablement, wire-shape parity with HTTP+SSE, synthetic-frame contract, error code mapping, TLS / coexistence recipes, Python adapter quick-start.

## v0.5.0 — earlier

**Status: shipped (2026-05-08).** Tag `v0.5.0` on `main`, merged via PR #15. The production-deployment unlock: Postgres `Store` adapter alongside SQLite (which stays first-class for compact installs), heartbeat sweeper + session-lock map GC, operator-facing CLI surface.

**What's in v0.5.0 (vs v0.4.0):**

- **Postgres `Store` adapter** (`internal/store/postgres/`) — full implementation over `pgx/v5` + `pgxpool`, embedded migrations via `golang-migrate/migrate v4`. Operator opts in via `storage.backend: postgres` (yaml) or `LOOMCYCLE_STORAGE_BACKEND=postgres` (env). Postgres ≥ 14 required.
- **SQLite stays first-class.** Default backend; both adapters validated against a shared behavioural contract suite (17 sub-tests) so they cannot drift silently.
- **Heartbeat sweeper** — periodic background goroutine marks runs whose process crashed mid-loop as `failed`. Default-on; `LOOMCYCLE_HEARTBEAT_*` env knobs control cadence + cutoff.
- **Session-lock map GC** — refcounted + idle-pruned; closes the v0.3.2 leak where the per-session continuation mutex map grew monotonically.
- **CLI subcommands** (`loomcycle <verb>`) — `validate`, `agents list`, `health`, `migrate up|down|status`, `migrate sqlite-to-postgres`. The migration tool copies an existing SQLite DB into Postgres with row-count + transcript-digest verification, idempotent on re-run.
- **Operator guide:** [POSTGRES.md](POSTGRES.md) covers configuration, the auto-migrate vs explicit-migrate policy split, the sqlite→postgres runbook, and reference benchmark numbers (100 concurrent agents: SQLite p99=31ms, Postgres p99=60ms — both well under the 1-second acceptance threshold).
- **`make pg-up` / `pg-down`** — Docker-based Postgres test fixture for local dev.

**Architecture decisions (vs original v0.5.0 plan):**

- **Cross-replica advisory locks deferred to v1.0.** Driver was multi-replica HA; for the only deployment shape today (single replica), the in-memory cancel registry works for both backends.
- **No live cutover for sqlite→postgres** — operator stops loomcycle, runs the copy, restarts. Live cutover is v1.0.

## v0.4.0 — earlier

**Status: shipped.** Tag `v0.4.0` on `main`. The runtime's MCP integration story is now production-validated against jobs-search-agent as the first real consumer.

**What's in v0.4.0:**

- Three providers — Anthropic Messages (with native `cache_control`), OpenAI Chat Completions, Ollama `/api/chat` (tool-tuned models only).
- Nine built-in tools — `Read`, `Write`, `Edit`, `HTTP`, `WebFetch`, `WebSearch`, `Bash`, `Agent` (sub-agent spawning), `Skill` (Approach A static bundling).
- MCP integration — pooled stdio children with auto-respawn, **Streamable HTTP transport** (Accept: both JSON + SSE per spec; SSE response decoder; per-call dial), per-server allowlists.
- **MCP startup retry** — exponential backoff handshake (500ms → 16s capped, 30s budget) so a peer compiling its `/api/mcp` route on first request doesn't get marked "skipped" indefinitely. Resolves the chicken-and-egg start-order race that bites every dev environment.
- Sub-agents via the `Agent` built-in — depth-capped (16), parent host policy + identity inherited via ctx.
- Agent tracking + cancel API — `agent_id` per run, cascade-cancel via `parent_agent_id`, list runs per user.
- Per-agent `max_tokens` config (output budget; covers the case where bundled skills + tool narration eat into a fixed cap).
- Anthropic driver: model alias plumbed from `message_start` into final `Usage.Model` so callers can price runs against the resolved alias.
- Sub-agent caller-host policy inheritance — children inherit the parent's per-call `allowed_hosts` instead of falling back to the operator's static list.
- SQLite store — sessions, runs, events; partial indexes for v0.4 sub-agent columns; WAL mode.
- Concurrency — global semaphore + bounded FIFO queue; backpressure → HTTP 429.
- TypeScript adapter (`@loomcycle/client`) shipped on npm.
- **End-to-end MCP-server validation against jobs-search-agent.** Two agents (`ats-filter`, `qa-agent`) now fetch context — and `qa-agent` also persists results — through typed `mcp__jobs__*` tools served by jobs-search-agent's own `/api/mcp` Streamable-HTTP server. This validates the runtime's MCP HTTP transport, host-policy inheritance, sub-agent retry, SSE response decoding, and Streamable-HTTP `Accept` handling against a real consumer. Per-agent migration in the consumer continues incrementally; the loomcycle surface is stable.

**Architecture pivot worth flagging.** v0.4.0 was originally planned as "migrate jobs-search-agent to the LocalAPI gateway" — feed an OpenAPI spec to loomcycle, register typed tools per operation. During the implementation, the user pulled back: the LocalAPI shape couples loomcycle's deploy config to one consumer's surface (loomcycle has to know about jobs-search-agent's routes). The cleaner architecture: jobs-search-agent runs its own MCP server; loomcycle stays domain-agnostic and consumes any MCP server via its existing HTTP MCP transport. v0.4.0 ships that pivot. LocalAPI remains in the codebase as a future-consumer convenience for OpenAPI-without-MCP-server cases, but it's not the integration vehicle.

**Scaffolded but not the v0.4 vehicle:**

- **LocalAPI MCP gateway** — code is in `internal/tools/localapi/`, parser + dispatcher wiring + unit tests all landed, registered into the dispatcher at boot when `cfg.LocalAPI.SpecPath` is non-empty. Useful for future consumers that have an OpenAPI spec and don't want to stand up an MCP server.

For usage: see [README](../README.md). For the architecture: see [ARCHITECTURE.md](ARCHITECTURE.md). For tool policy: see [TOOLS.md](TOOLS.md).

## v0.7.x — near-term

Items scoped after v0.7.0 ships, ordered roughly by readiness. Distinct from the v1.0 outline below: these are bounded chunks of work with a known shape, not framework-defining primitives.

### Shipped post-v0.7.0

- **DeepSeek thinking-mode roundtrip** (PR #25, 2026-05-08). DeepSeek V4 Pro / deepseek-reasoner returns `reasoning_content` alongside `content`; the API requires it to be echoed back on subsequent turns or the next request 400s. The OpenAI driver now captures the reasoning trace, surfaces it on `EventDone.Reasoning`, and the request builder serialises it back when the assistant `Message` carries one.
- **Ollama qwen3 tool-call-as-text recovery** (PR #26, 2026-05-08). qwen3:14b sometimes loses tool-call envelope discipline mid-conversation and emits the next call as plain JSON content. The Ollama driver detects the JSON shape at end-of-stream and synthesises an `EventToolCall` so the loop iterates instead of terminating with the JSON dump as the final answer. JSON-shape and array-of-calls forms covered; the bracketed `[tool_use: name]` notation is not.
- **`TestBashTimeout` race-detector reliability** (PR #27, 2026-05-08). Moved the timing-sensitive timeout test behind a `//go:build !race` tag — the race detector's goroutine-scheduling overhead starves the kill goroutine long enough that the full `sleep 5` runs to completion. Production code is fine; the race environment isn't a useful place to validate timing-sensitive scheduling.
- **Per-token text coalescing for OpenAI / DeepSeek** (PR #28, 2026-05-09). Streaming text deltas accumulate into 64-byte chunks before emitting `EventText`, with mandatory flushes on newline / before tool_calls / end-of-stream. Closes the "every word on its own line" cosmetic noise DeepSeek's tokenizer produced in line-prefix-logging consumers.
- **SSE wire-level keepalive** (PR #29, 2026-05-09). Long-lived agent streams emit `: keepalive\n\n` comment frames every 20 s by default to keep the underlying TCP/HTTP path warm. Closes the opaque `TypeError: terminated` undici reports when networks with idle-connection timeouts (Tailscale, NAT routers) drop a silent stream. Configurable via `LOOMCYCLE_SSE_KEEPALIVE_MS`; 0 disables.
- **Parallel tool dispatch** (PR #30, 2026-05-09). The agent loop dispatched a turn's tool_calls serially — for the `Agent` built-in tool that turned 3-way fan-outs into queues. New `executePendingTools` runs each tool_call in its own goroutine, bounded by `LOOMCYCLE_TOOL_PARALLELISM` (default 8). Messages handed back to the model preserve tool_call order; SSE events emit in completion order so live consumers see fast tools' results first.
- **`EventThinking` event type** (PR #32, 2026-05-09). Live streaming of the model's reasoning trace as a typed event distinct from `EventText`. All three drivers wire it up: Anthropic from `thinking_delta` content blocks, OpenAI / DeepSeek from `delta.reasoning_content`, Ollama from `message.thinking`. Consumers can render or hide the trace independently of the user-visible answer; `EventDone.Reasoning` still carries the consolidated trace for next-turn echo (DeepSeek roundtrip requirement).
- **Tool-use hooks** (PR — pending merge of this branch, 2026-05-09). Operator-supplied middleware around tool dispatch. External apps register HTTP-webhook callbacks against `(agent, tool, phase)` selectors via `POST /v1/hooks`; loomcycle invokes them around `executeTool` so a hook can rewrite the input, short-circuit with a synthetic result, or rewrite the post-tool result. Per-`(owner, name)` idempotent registration prevents cascading on app restart. Fail-open default (telemetry hooks don't block); fail-closed available for security-shaped hooks. See `docs/TOOLS.md` for the full surface.

### Still queued

- **jobs-search-agent provider routing rollout.** v0.6.0 shipped the per-agent `provider:` knob; v0.7.0 added tier resolution. The consumer-side flip happens in `jobs-search-agent`'s agent yaml: public-data agents (ATS filtering, position relevance, company profiling) move to `tier: low` or `provider: deepseek`; CV / CL generation stays explicit on Anthropic. The cv-adapter / ai-detector pair specifically uses tier+per-agent-overrides to enforce different model families for the AI-review pipeline. Validation: cost rollups by `runs.model` should show DeepSeek dominating volume while Anthropic dominates spend.
- **Resolver Snapshot endpoint.** v0.7.0 ships `Resolver.Snapshot()` for in-process introspection but no HTTP / gRPC surface to expose it. A small admin endpoint (`GET /v1/_resolver`) gated by the auth bearer would let dashboards render the matrix state without log scraping. Bounded scope; defer until the first consumer asks.
- **Ollama markdown-notation tool-call parser.** Follow-up to PR #26: the JSON-shape recovery covers the common qwen3 failure mode but doesn't catch the `[tool_use: name]` bracketed-markdown form some chat templates produce. Low priority — only matters when running models that emit that style.

## v1.0 — planned

The v1.0 ambition is a **high-load agentic runtime**: 10,000 concurrent agents and sub-agents on a single replica, MCP-orchestrable from external tools (Claude Code, other harnesses), with first-class agent self-improvement through persistent memory and inter-agent messaging.

These items are **outline-only** here. Detailed design (API schemas, storage shapes, retention semantics) lives in feature-branch RFCs at implementation time, not in this roadmap.

### Framework primitives

#### Memory tool

Agent-scoped persistent storage. Backs self-improving agents — an agent can write a fact, a learned heuristic, a counter, or a per-user preference, and read it back on the next invocation. Scope variants (per-agent vs per-session vs per-user) and retention semantics (TTL, size caps, eviction policy) shape the storage backend, which lands on the v1.0 Postgres `Store` adapter.

What's not yet decided: the API shape (key/value vs append-log vs both), TTL semantics, encryption-at-rest, cross-agent read permission model, schema versioning for stored values. RFC at pickup.

#### Channel tool

Persistent inter-agent message bus. One agent writes to a named channel; another reads from it. Powers patterns like "researcher writes findings into `findings/` channel; analyst drains the channel and produces summaries" without making the orchestrator handle handoff. Backend likely Postgres LISTEN/NOTIFY or Redis Streams; durability guarantees TBD.

What's not yet decided: queue vs topic semantics, durability vs at-most-once, ACL (which agent can publish to which channel), backpressure when readers fall behind, dead-letter handling. RFC at pickup.

#### LoomHelp tool

Runtime introspection. Agents query "what tools do I have, what message formats, what skills are loaded, what's the agent_id of my parent" via a single read-only tool. Useful for the Comfort Agents pattern — agents that build their own task plans benefit from being able to inspect their environment before deciding what to do. Output schema is operator-config-derived, with secrets explicitly excluded.

What's not yet decided: output format (JSON schema vs markdown vs both), what counts as a "secret" beyond env-var name patterns, whether the tool can be selectively narrowed at the agent layer.

#### LoomCycle MCP

Loomcycle exposes itself as an **MCP server**. External orchestrators (Claude Code, agentic harnesses, other loomcycle instances) connect to it as an MCP client and:

- Configure agents and spawn runs (alternate front-end to `/v1/runs`).
- Send messages on Channels.
- Read/write Memory entries.
- Call LoomHelp.
- Subscribe to run-event streams (alternate to SSE).

This is the "MCP-configurable" axis: instead of writing YAML and POSTing JSON, an external tool drives loomcycle through standard MCP. Surface area maps roughly 1:1 to the existing `/v1/*` endpoints, with auth via the operator's bearer token translated into MCP's auth scheme.

What's not yet decided: stdio vs HTTP transport (probably both), method naming (resources vs tools), whether MCP clients can register new agents at runtime or only spawn from operator-defined ones, handling of long-lived run streams across MCP's request/response shape.

#### Tool-use hooks (PreToolUse / PostToolUse)

Operator-supplied middleware around tool dispatch. The agent loop calls `PreToolUse` before invoking any tool — given the tool name, the model's input, the agent identity, and the request context, the hook can rewrite the input, deny the call (returning a synthetic `tool_result`), audit it, or annotate it with metadata (e.g. a trace span ID). After the tool runs, `PostToolUse` sees the result and can rewrite it. The canonical use case for the post-hook is wrapping untrusted content from `WebFetch` / `HTTP` / MCP results in a trust-boundary marker so a downstream LLM treats the payload as data rather than instructions — the v0.2 plan called this the "untrusted-content wrap."

Hooks are the seam for cross-cutting concerns the runtime currently has no first-class place for: per-tool quotas, audit logs that capture every tool call before the policy layer touches it, content-sanitization passes, OTEL spans tied to tool invocations, soft-deny patterns ("you tried to fetch X; here's a redacted version instead"). Today every one of these would have to be bolted into the dispatcher; hooks let them be operator-pluggable without forking the runtime. The pre/post split mirrors the same pattern in Claude Code's hooks system, so operators porting policy code between the two have one mental model.

This was originally a v0.2 plan item, deferred at v0.4 because no consumer required it, and resurfaces alongside the v1.0 observability work — many of the items in *High-load runtime work* below (OTEL spans, per-tool quotas, audit) want hooks as their wiring point.

What's not yet decided: hook composition order (single chain vs typed phases), how hooks express denial vs annotation, whether hooks see the post-policy-narrowing input or the raw model input, whether hooks can short-circuit the loop entirely, and the wire shape — Go interface (compile-time, in-process), HTTP webhook (operator-supplied service), or MCP-callable (agentic hook driving agentic policy). RFC at pickup.

### High-load runtime work

These are cross-cutting capacity items. Not a single feature; collectively they take the runtime from "single-tenant comfortable on a 4–8 GiB VPS" to "10k concurrent agents per replica."

- **Per-tenant fairness** in the concurrency layer. Currently every caller competes for one global semaphore — a noisy tenant monopolises the pool. Token bucket per `user_id` (or per `tenant_id` once that lands), with a small unfair share for global priorities.
- **Postgres `Store` adapter** replaces single-writer SQLite for multi-replica HA. Schema is already shaped to absorb this (the `Store` interface is provider-agnostic; current SQLite implementation is one of N possible backends).
- **In-memory run-status cache.** Today every `GET /v1/agents/{agent_id}` hits SQLite. At 10k concurrent runs this is a hot path. LRU keyed on `agent_id` with sub-second TTL.
- **Session-lock map GC.** The HTTP server's `sync.Map` of session locks never garbage-collects entries (~32 B per session). Periodic sweep + bounded total entries.
- **OpenTelemetry traces + Prometheus metrics endpoint.** Currently logs only. Per-run trace, per-tool-call span, request rate / queue depth / semaphore-occupancy / provider-RTT histograms.
- **Heartbeat sweeper.** `last_heartbeat_at` is updated by the loop on every iteration but nothing reads it. A sweeper detects crashed runs (no heartbeat for > N minutes) and marks them failed so they don't stay `running` forever.
- **Multi-replica HA.** Postgres for transcripts; Redis for in-flight cancel registry replication. Out-of-process cancel works across replicas via Redis pub-sub.

### Operator posture: sandbox vs agentic, no profile flag

v1.0 keeps a single binary and a single config schema. There is **no** `--profile=sandbox` flag and no separate `loomcycle-server` / `loomcycle-agentic` binaries. Operators compose their own posture by setting individual env vars and YAML keys; the docs ship a cookbook of common shapes.

**Sandbox recipe** — server-style deployment, agents process untrusted prompts, no host filesystem reach:

```bash
LOOMCYCLE_BASH_ENABLED=0
LOOMCYCLE_HTTP_HOST_ALLOWLIST=             # empty — agents reach no hosts unless caller supplies allowed_hosts
LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1      # caller's per-request allowed_hosts is the policy
LOOMCYCLE_READ_ROOT=                       # unset → Read tool refuses every call
LOOMCYCLE_WRITE_ROOT=                      # unset → Write/Edit refuse every call
LOOMCYCLE_SKILLS_ROOT=/srv/loomcycle/skills
```

YAML: every agent's `allowed_tools` lists only the network-restricted set (`HTTP` only with caller-supplied hosts, `WebSearch` with `web_search_filter: drop`, `Agent` for orchestration). Run inside a container; rely on the container for true filesystem isolation.

**Agentic recipe** — the runtime has read/write access to a real working directory, can run shell commands, reaches a broad set of hosts:

```bash
LOOMCYCLE_BASH_ENABLED=1                   # bash on (still NOT a real sandbox; container required)
LOOMCYCLE_BASH_CWD=/work
LOOMCYCLE_HTTP_HOST_ALLOWLIST=api.anthropic.com,api.brave.com,github.com,...
LOOMCYCLE_READ_ROOT=/work
LOOMCYCLE_WRITE_ROOT=/work
LOOMCYCLE_SKILLS_ROOT=/work/.claude/skills
LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST=localhost,127.0.0.1
```

YAML: agents can list any tool. The bearer token (`LOOMCYCLE_AUTH_TOKEN`) is the trust boundary; treat anyone with the token as fully trusted to drive the runtime.

The cookbook in v1.0 will expand this into a full set: development sandbox, single-user agentic dev, multi-tenant SaaS, batch processing, etc.

### Web monitoring frontend

A small frontend on top of the SQLite/Postgres event stream — see runs, drill into transcripts, view token + cost rollups. Not a chat UI; this is operator-facing monitoring. Distinct from the LoomCycle MCP, which is for external orchestration.

## Decision principles

These hold across v1.0 work; deviation requires a written justification:

- **Config-driven posture, no profile flag.** Operators compose their security stance from individual env + YAML keys. We do not ship a "sandbox mode" abstraction.
- **One binary stays one binary.** No `loomcycle-server` vs `loomcycle-agent`. Every feature lands in `cmd/loomcycle`. Build artefacts stay singular.
- **MCP-orchestrable.** Whatever surface we expose for agents (Memory, Channel, LoomHelp), we also expose to external MCP clients. Agents and orchestrators play on the same plane.
- **Storage is pluggable.** SQLite for dev/single-tenant; Postgres for multi-replica. Anything new (Memory, Channel) goes through the `Store` interface, not direct SQL.
- **No vendor SDKs in the loop.** Every provider driver is pure HTTP. No bundled binaries; no subprocess auth inheritance.
- **Default-deny stays default-deny.** New tools start invisible to existing agents until they opt in.

## How to contribute to v1.0

Pick an item, write an RFC (one markdown file under `doc-internal/rfcs/<feature>.md`), open a feature branch (`feature-<name>`), follow the chain documented in `CLAUDE.md` (architect → plan → code → tests → review → merge). The RFC is the design step — implementation follows once the RFC is reviewed.

For non-trivial items (Memory tool, Channel tool, Postgres adapter), the RFC should cover:

1. The user-visible surface (API shape, semantics, error cases).
2. The storage / wire shape (schema, message formats).
3. Trust model — who can call this, what's the threat case.
4. Migration plan — what existing code path changes, what stays compatible.
5. Verification — how an operator confirms the feature works end-to-end.

Small features (a new built-in tool, a new provider driver, a fix) skip the RFC and go straight to a feature branch.
