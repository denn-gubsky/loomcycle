<p align="center">
  <img src="docs/assets/logo.png" alt="loomcycle" width="640" />
</p>

<p align="center">
  <strong>A high-load agentic runtime — one Go sidecar that owns the LLM tool-use loop end-to-end.</strong>
</p>

<p align="center">
  <a href="https://github.com/denn-gubsky/loomcycle/releases"><img alt="release" src="https://img.shields.io/github/v/tag/denn-gubsky/loomcycle?label=release"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <img alt="go" src="https://img.shields.io/badge/go-1.22%2B-00ADD8">
</p>

---

> **🚧 Closed for external contributions until v1.x.** Loomcycle is in active v0.8 → v0.9 → v1.0 development. Pull requests will be acknowledged but closed without merge during this window. Bug reports for clear-cut defects and security disclosures are still welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the policy and the trigger conditions for reopening.

---

## What it is

LoomCycle is a single Go binary that runs as a local sidecar and serves an HTTP+SSE API to your application. It owns the model→tool_use→tool_result→model loop, talks directly to provider HTTP APIs (no vendor SDK in the loop, no bundled binary), and dispatches tool calls to built-ins, MCP servers, or operator-supplied OpenAPI gateways. Multi-tenant. Multi-provider. Multi-agent (parents spawn sub-agents).

It exists to replace bundled-binary agent SDKs that cold-start in 20–30 s, leak memory under load, and lock you into one provider — the things that made the first production user (`jobs-search-agent`) infeasible to scale on a small VPS.

## Why this approach

- **Pure HTTP loop.** No vendor binary spawned per call. The runtime is one Go process, ~16 MB compiled, single static binary. Cold-start is the kernel's exec time.
- **Provider-agnostic.** Five drivers — Anthropic Messages, OpenAI Chat Completions, DeepSeek (OpenAI-compatible), Google Gemini (`generateContent`), Ollama `/api/chat` — all normalize to one `Event` channel the loop drains. Capability flags expose provider-specific extras (Anthropic `cache_control`, Gemini's 2 M context, OpenAI / DeepSeek / Gemini parallel tool calls).
- **Per-agent provider routing.** YAML `provider:` field per agent lets a consumer mix backends by data sensitivity: Anthropic for user-sensitive paths, DeepSeek / Gemini for high-volume public-data work, Ollama (local llama) for offline / cost-floor scenarios. Same wire surface, different cost / privacy posture per agent.
- **Default-deny tool policy.** Every built-in is disabled until env-configured. Every agent gets zero tools until `allowed_tools` is set in YAML. Two layers must say "yes" before a tool reaches the model.
- **Native cache placement.** When the provider supports it (Anthropic), system blocks marked `cacheable: true` carry `cache_control` on the wire — you keep cache reads on the stable preamble even when the rest of the conversation churns.
- **Two wire surfaces + a Web UI.** HTTP+SSE (default), gRPC (opt-in via `LOOMCYCLE_GRPC_ADDR`), and an embedded read-only React SPA at `/ui` for monitoring agent runs (parent → children tree, per-agent transcript log, cancel button). All share the same store, cancel registry, runner, and concurrency semaphore — a cancel issued via the UI reaches a run started via HTTP and vice versa.
- **Observable everywhere.** Every text chunk, tool call, tool result, usage update, retry, and reasoning fragment is an SSE / gRPC event. Nothing happens silently.

## Quick start

```bash
# 1. Build (UI + binary in one shot)
make build-all
# Or Go-only (skips embedding the UI; /ui returns 503):
#   make build

# 2. Configure
cp .env.example .env.local       # set ANTHROPIC_API_KEY / GEMINI_API_KEY / etc.
cp loomcycle.example.yaml ~/.config/loomcycle/loomcycle.yaml

# 3. Run
./bin/loomcycle --config ~/.config/loomcycle/loomcycle.yaml

# 4. Smoke
curl http://127.0.0.1:8787/healthz
# {"ok":true}

# 5. Real call (from another terminal)
curl -N http://127.0.0.1:8787/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "agent": "default",
    "segments": [{"role":"user","content":[{"type":"trusted-text","text":"Hello"}]}]
  }'

# 6. Open the Web UI (one-time per browser session)
open "http://127.0.0.1:8787/ui?token=$LOOMCYCLE_AUTH_TOKEN"
# Sets a HttpOnly session cookie + redirects to /ui.
# Pick a user_id from the dropdown to see runs.
```

## What's in v0.8.0

| Surface             | Status |
|---------------------|--------|
| **`Memory` built-in tool** | ✅ Persistent agent-scoped key/value storage that survives across runs and sessions. Five ops behind one tool: `get` / `set` / `delete` / `list` / **`incr`** (atomic counter). Two scopes: `agent` (yaml-keyed; cross-run, shared across users) and `user` (user_id-keyed; cross-agent, per end-user). Backed by a new `memory` table on both SQLite and Postgres adapters. (PR #45) |
| **Per-agent yaml policy** | ✅ `memory_scopes: [agent, user]` is a default-deny allowlist — `Memory` in `allowed_tools` is necessary but not sufficient. Optional `memory_quota_bytes` per-agent override of the global `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` cap. Sub-agents get their OWN policy from yaml — the parent's `memory_scopes` does NOT cascade. (PR #45) |
| **Web UI Memory page** | ✅ `/ui/memory` — three-pane browser: scope picker → scope_id list with key counts and byte totals → keys with prefix filter → entry detail with pretty-printed JSON, timestamps, and TTL. Polls the new `/v1/_memory/*` admin endpoints on a 5 s tick. (PR #45) |
| **Admin API for Memory** | ✅ Four read-only routes — `GET /v1/_memory/scopes`, `/scopes/{scope}`, `/scopes/{scope}/{scope_id}/keys`, `/scopes/{scope}/{scope_id}/keys/{key...}`. Bearer-authed via the existing middleware. The `{key...}` multi-segment route handles slashed keys like `events/2026-05-09T10:00`. (PR #45) |
| **Concurrency hardening** | ✅ Atomic increment correctness verified by a 100-goroutine regression test on both backends. Caught and fixed two real lost-update races at review time: SQLite `BeginTx(nil)` is DEFERRED (fix: pinned connection + raw `BEGIN IMMEDIATE`); Postgres `SELECT FOR UPDATE` doesn't lock absent rows (fix: `pg_advisory_xact_lock` keyed by hash of the (scope, scope_id, key) tuple). (PR #45) |
| **Pre-existing host-policy fix** | ✅ `handleMessages` (session continuation path) had been missing `tools.WithHostPolicy` on its loop ctx since v0.4.0 — sub-agents from continuations fell back to the operator's static allowlist instead of the caller's narrowed list. Fixed alongside the new Memory ctx values. (PR #45) |

## What's in v0.7.4

| Surface             | Status |
|---------------------|--------|
| **Web UI agent name + content fixes** | ✅ Run list now shows the YAML-declared agent name (`qa-agent`, `company-researcher`) instead of just the UUID. Agent detail header reads from the corrected wire shape (model + tokens + duration). Transcript event panels now render actual content (text, tool calls, tool results, errors) — collapsed-by-default with a one-line summary, click to expand for full text + tool params + pretty-printed JSON. (PRs #41, #42) |
| **User picker dropdown** | ✅ New `GET /v1/_users` admin endpoint surfaces distinct user_ids that have runs in the store, with running / total counts + last-active timestamp. The Web UI top bar swaps the freeform user_id input for a dropdown — operators no longer need to know the UUID up front. Manual override (✎ button) preserved for picking a user who has no runs yet. (PR #40) |
| **Gemini config validation hotfix** | ✅ v0.7.2 wired the Gemini driver into the resolver but missed adding `gemini` to the config validator's allowlist; operators with `provider: gemini` rows in their yaml saw startup fail. Fixed. (PR #39) |

## What's in v0.7.3

| Surface             | Status |
|---------------------|--------|
| **Embedded read-only Web UI** | ✅ React 19 + Vite 7 + TypeScript SPA at `/ui`. Two pages: run list (parent → children tree, status filter, auto-refresh every 3 s) and per-agent detail (event log: text / thinking / tool_call / tool_result / error / retry / done; auto-refresh every 1.5 s for active runs; cancel button). No new wire endpoints — the SPA reuses the existing `/v1/users/{user_id}/agents`, `/v1/agents/{agent_id}`, `/v1/sessions/{id}/transcript`, `/v1/agents/{agent_id}/cancel` routes. |
| **Bearer-in-cookie auth** | ✅ Operator visits `/ui?token=<bearer>` once; server sets a `loomcycle_session` HttpOnly cookie and 302s back. Subsequent /v1 calls authenticate via the cookie (same-origin fetch). The existing `Authorization: Bearer …` header path keeps working unchanged for adapters / curl / SDKs — bearer wins on precedence so a stale cookie can't mask a deliberate request. |
| **Build pipeline** | ✅ `make build-ui` runs `npm install + npm run build` and writes the production bundle to `internal/webui/dist/` (embedded via `go:embed`). `make build-all` does both. A fresh checkout without npm toolchain still compiles via Go alone (a committed `.gitkeep` placeholder); `/ui` then returns 503 with a `ui_not_built` code as the diagnostic. |

## What's in v0.7.2

| Surface             | Status |
|---------------------|--------|
| **Google Gemini provider** | ✅ Fifth backend driver in `internal/providers/gemini/`. Speaks Gemini's `generateContent` API: model name in URL path (not body), `x-goog-api-key` header auth, SSE streaming via `?alt=sse`. Tool dispatch maps loomcycle's `tool_use` / `tool_result` to Gemini's `functionCall` / `functionResponse` content parts. |
| **Effort hint translation** | ✅ `effort: low \| medium \| high` maps to `generationConfig.thinkingConfig.thinkingBudget` on gemini-2.5-flash / gemini-2.5-pro: `low` → 0 (disable), `medium` → 2048, `high` → 8192 (clamped to `max_tokens - 1024` when needed). Same vocabulary as Anthropic / OpenAI — no per-provider effort dialect. |
| **Resolver matrix integration** | ✅ Excluded when `GEMINI_API_KEY` is unset; probed at startup and on the periodic re-probe with the same 5 s deadline as the others. Per-agent yaml: `provider: gemini` and `model: gemini-2.5-flash` (or any model the wire `/v1beta/models` returns). |
| **Vertex AI deployments** | ✅ Optional `GEMINI_BASE_URL` overrides for Vertex AI Gemini gateways (production deployments routing through GCP project quotas instead of the public AI Studio API). |

## What's in v0.7.1

| Surface             | Status |
|---------------------|--------|
| **`EventThinking` event type** | ✅ Live streaming of model reasoning as a typed event distinct from `EventText`. Anthropic from `thinking_delta` content blocks; OpenAI / DeepSeek from `delta.reasoning_content`; Ollama from `message.thinking`. `EventDone.Reasoning` still carries the consolidated trace for next-turn echo (DeepSeek roundtrip). |
| **Tool-use hooks** | ✅ Operator-supplied middleware around tool dispatch via `POST /v1/hooks`. Selectors filter by `(agent, tool, phase)`; per-`(owner, name)` idempotent registration prevents cascading on app restart. Fail-open default (telemetry hooks don't block); fail-closed available for security-shaped hooks. See [`docs/TOOLS.md`](docs/TOOLS.md). |
| **Resolver Snapshot endpoint** | ✅ `GET /v1/_resolver` exposes the in-process availability matrix as JSON, bearer-authed. 503 with `resolver_unavailable` in the brief degraded-startup window so dashboards distinguish "matrix not available" from "matrix is empty". |
| **Parallel tool dispatch** | ✅ The agent loop dispatched a turn's tool_calls serially — `Agent` fan-outs queued. New `executePendingTools` runs each in its own goroutine, default 8 concurrent, `LOOMCYCLE_TOOL_PARALLELISM` to override. |
| **SSE wire-level keepalive** | ✅ Long-lived agent streams emit `: keepalive\n\n` comment frames every 20 s by default. Closes the opaque `TypeError: terminated` undici reports when networks with idle-connection timeouts (Tailscale, NAT routers) drop a silent stream. `LOOMCYCLE_SSE_KEEPALIVE_MS` to override; 0 disables. |
| **Per-token text coalescing** | ✅ OpenAI / DeepSeek streaming text deltas accumulate into 64-byte chunks. Closes the "every word on its own line" cosmetic noise DeepSeek's tokenizer produced. |
| **Ollama qwen3 tool-call recovery** | ✅ Both JSON-shape (`{"name":"...","arguments":{...}}`) and bracketed-markdown (`[tool_use: name]\n{args}`) forms now synthesize `EventToolCall` so the loop iterates instead of terminating with the markup as the final answer. |
| **DeepSeek thinking-mode roundtrip** | ✅ DeepSeek V4 Pro / deepseek-reasoner returns `reasoning_content` alongside `content`; the API requires it echoed back on subsequent turns. The OpenAI driver captures it on `EventDone.Reasoning`; the request builder serialises it back when the assistant Message carries one. |

## What's in v0.7.0

| Surface             | Status |
|---------------------|--------|
| **Tier-based resolution** | ✅ Agents declare `tier: low \| middle \| high` instead of pinning a specific model. Resolver picks `(provider, model)` against a live availability matrix. Per-agent `providers:` and `models:` overrides cover asymmetric pinning. Explicit pins from v0.6.x continue to work. |
| **Live `/v1/models` probes** | ✅ Each driver implements `Probe` + `ListModels`. Startup probes run in parallel with a 5s deadline; periodic re-probe runs every 15 min (configurable up to 1h via `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS`). |
| **`Excluded` flag**  | ✅ Providers without API keys are explicitly marked excluded in the matrix — distinct from "probe attempted, failed". Visible in `Resolver.Snapshot()` for dashboards. Startup logs surface the state. |
| **Reactive stall feedback** | ✅ Loop calls `MarkStalled` on driver errors (5xx after retry, mid-stream errors). Resolver skips stalled `(provider, model)` pairs until next probe revives. `ctx.Err()` guards prevent user-cancellations from polluting the matrix. |
| **Per-driver effort hint** | ✅ Agent yaml: `effort: low \| medium \| high`. Anthropic → `thinking.budget_tokens` (haiku always skips); OpenAI → `reasoning_effort`; DeepSeek inherits OpenAI; Ollama is a no-op. Loop logs once per Run when effort is dropped. |

**See [`docs/PLAN.md#v070--current`](docs/PLAN.md#v070--current) for the full feature breakdown and the cv-adapter / ai-detector example showing how to enforce different model families across a verification pipeline.**

## What's in v0.6.0

| Surface             | Status |
|---------------------|--------|
| **DeepSeek provider** | ✅ Wraps the OpenAI driver with the DeepSeek base URL pre-baked. Per-agent yaml: `provider: deepseek`. Set `DEEPSEEK_API_KEY`; optional `DEEPSEEK_BASE_URL` for self-hosted OpenAI-compatible mirrors (vLLM, etc.). |
| **OpenAI `Usage.Model` fix** | ✅ Driver now captures the wire-resolved model alias from the streamed chunk envelope, so `runs.model` populates for every OpenAI-compatible run (OpenAI itself, DeepSeek, vLLM). Same regression class as the v0.4 anthropic fix; latent until the DeepSeek live test surfaced it. |
| **Ollama live integration tests** | ✅ Three tests (probe, chat, tool call) gated by `OLLAMA_TEST_BASE_URL`. Validated against qwen3:14b on RTX 5080 (16GB VRAM) end-to-end as the offline / cost-floor backend. |
| **Constant-time bearer compare** | ✅ New `internal/auth.CompareBearer` (sha256+CTC) replaces raw `subtle.ConstantTimeCompare` on both HTTP and gRPC. Closes a length-leak side channel that the stdlib documents but doesn't fix. |

**Provider routing intent (jobs-search-agent first):** Anthropic for user-sensitive paths · DeepSeek for high-volume public data · Ollama (local llama) for offline / cost floor · OpenAI for general use / prototyping. See [`docs/PLAN.md`](docs/PLAN.md#v060--current) for the full rationale and the v0.7+ rollout plan.

## What's in v0.5.5

| Surface             | Status |
|---------------------|--------|
| **gRPC server**      | ✅ Opt-in via `LOOMCYCLE_GRPC_ADDR`. All seven RPCs mirror the HTTP+SSE surface 1:1 (`Run`, `Continue`, `GetAgent`, `CancelAgent`, `ListUserAgents`, `GetTranscript`, `Health`). Coexists with HTTP — same store, same cancel registry, same semaphore. See [`docs/GRPC.md`](docs/GRPC.md). |
| **Python adapter**   | ✅ `pip install loomcycle`. Async `LoomcycleClient` over `grpc.aio` covering all seven RPCs. PEP-561 `py.typed`. |
| **`internal/runner/`** | ✅ Wire-agnostic seam — HTTP server satisfies `runner.Runner`, gRPC server delegates to the same instance. |
| **Synthetic registration frames** | ✅ Wire-stable `session` + `agent` frame pair at the head of every Run/Continue stream so adapters capture `(agent_id, run_id, session_id, parent_agent_id)` without re-decoding the transcript. |

## What's in v0.5.0

| Surface             | Status |
|---------------------|--------|
| **Postgres backend** | ✅ Full `Store` adapter over `pgx/v5` + embedded `golang-migrate`. Same interface as SQLite; adapters share a contract suite so they can't drift. See [`docs/POSTGRES.md`](docs/POSTGRES.md). |
| **SQLite stays first-class** | ✅ Default backend; both adapters tested against the same behavioural contract suite. |
| **Heartbeat sweeper** | ✅ Periodic background goroutine marks runs whose process crashed mid-loop as `failed`. Default-on, env-tunable. |
| **Session-lock map GC** | ✅ Refcounted + idle-pruned; closes the v0.3.2 leak where `sessionLocks` grew monotonically. |
| **CLI subcommands**  | ✅ `loomcycle validate` · `agents list` · `health` · `migrate up\|down\|status` · `migrate sqlite-to-postgres` |
| **`make pg-up` / `pg-down`** | ✅ Local Postgres fixture for tests + dev. |

The bulk of v0.5.0 is operational: backbone you'll need before scaling past one replica. SQLite stays the default for compact installs.

## What's in v0.4.0

| Surface             | Status |
|---------------------|--------|
| **Providers**       | Anthropic ✅ · OpenAI ✅ · Ollama ✅ (tool-tuned models only). DeepSeek added in v0.6.0. |
| **Built-in tools**  | Read · Write · Edit · HTTP · WebFetch · WebSearch · Bash · **Agent** · **Skill** |
| **MCP transports**  | stdio (pooled, auto-respawn) · HTTP (Streamable, SSE-aware) |
| **MCP startup retry** | Exponential backoff handshake on boot — handles peer-still-starting races |
| **LocalAPI gateway** | ⏳ scaffolded — useful for consumers that have an OpenAPI spec but don't want to stand up an MCP server. Not the v0.4 integration vehicle (jobs-search-agent migrated to the MCP-server pattern instead — see below). |
| **Sub-agents**      | Agent built-in spawns child runs; depth-capped; parent host policy + identity inherit via ctx |
| **Skills**          | Approach A: static bundling at config-load (skill body concatenated into agent system prompt) |
| **Storage**         | SQLite (modernc.org, pure Go); sessions / runs / events tables; partial indexes for v0.4 sub-agent columns |
| **Concurrency**     | Global semaphore + bounded FIFO queue; backpressure → HTTP 429 |
| **Cancellation**    | Registry-based cancel API; cascades from parent to all children via `parent_agent_id` walk |
| **Adapters**        | TypeScript (`@loomcycle/client`) ✅ · Python ⏳ deferred |

> **v0.4.0 — released after end-to-end MCP integration with jobs-search-agent.** Two agents (`ats-filter`, `qa-agent`) now fetch context — and `qa-agent` also persists results — through typed `mcp__jobs__*` tools served by jobs-search-agent's own MCP server. This validates the runtime's MCP HTTP transport, host-policy inheritance, sub-agent retry, SSE response decoding, and Streamable-HTTP `Accept` handling against a real consumer. Per-agent migration in the consumer continues incrementally; the loomcycle surface is stable.

## Architecture (one diagram)

```
                  ┌──────────────────────────────────────────────┐
  Next.js  ───┐   │  loomcycle (Go, single binary)               │
              │   │                                              │
  Python   ───┼──▶│  HTTP+SSE API   ◀── auth ── /healthz         │
              │   │     │                                        │
  CLI      ───┘   │     ▼                                        │
                  │  Concurrency semaphore (FIFO + backpressure) │
                  │     │                                        │
                  │     ▼                                        │
                  │  Agent loop ──── Provider drivers            │
                  │     │              ├─ Anthropic   ✅         │
                  │     │              ├─ OpenAI      ✅         │
                  │     │              ├─ DeepSeek    ✅         │
                  │     │              └─ Ollama      ✅         │
                  │     ▼                                        │
                  │  Tool dispatcher                             │
                  │     ├─ Built-ins (9 tools)                   │
                  │     ├─ MCP layer (stdio + HTTP)              │
                  │     ├─ LocalAPI (OpenAPI → tools)            │
                  │     └─ Agent tool → sub-agent runner         │
                  │     ▼                                        │
                  │  Cache (Anthropic native; response KV ⏳)    │
                  │     ▼                                        │
                  │  Store (SQLite ✅ default; Postgres ✅ v0.5)│
                  └──────────────────────────────────────────────┘
```

## Configuration cheatsheet

Most-used knobs (full list in `.env.example` + `loomcycle.example.yaml`):

| Env / YAML | What it does |
|---|---|
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `DEEPSEEK_API_KEY` / `OLLAMA_BASE_URL` | Provider credentials. Set what you'll use; unset keys disable the corresponding driver. `DEEPSEEK_BASE_URL` overrides the public DeepSeek endpoint for self-hosted OpenAI-compatible mirrors. |
| `LOOMCYCLE_AUTH_TOKEN` | Bearer token required on every `/v1/*` request. Empty = dev-mode unauthenticated (warning logged). |
| `LOOMCYCLE_LISTEN_ADDR` | Default `127.0.0.1:8787`. |
| `LOOMCYCLE_DATA_DIR` | SQLite store location. Default `./data`. |
| `LOOMCYCLE_READ_ROOT` / `LOOMCYCLE_WRITE_ROOT` | Sandbox roots for Read / Write / Edit. Empty = tool refuses every call. |
| `LOOMCYCLE_HTTP_HOST_ALLOWLIST` | Comma-separated suffix-matched allowlist for HTTP / WebFetch. Empty = HTTP refuses. |
| `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST` | Loopback-IP exemption (e.g. `localhost,127.0.0.1`) for agents calling back to a local API. Each entry must also be on `LOOMCYCLE_HTTP_HOST_ALLOWLIST`. |
| `LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE` | `1` flips per-request `allowed_hosts` from intersection-only narrowing to "caller is the policy" (operator's static list becomes a fallback for runs that don't carry their own list). |
| `LOOMCYCLE_BASH_ENABLED` / `LOOMCYCLE_BASH_CWD` | Bash is `0` by default. NOT a true sandbox even when enabled — see Security. |
| `BRAVE_API_KEY` | Enables WebSearch (Brave Search). |
| `LOOMCYCLE_SKILLS_ROOT` | Directory of skill bundles (`<name>/SKILL.md`). Skills land in agent system prompts via the `skills:` list in YAML. |
| YAML `defaults.provider`, `defaults.model`, `agents.<name>.allowed_tools`, `concurrency.max_concurrent_runs` | Operator policy and per-agent shape. See `loomcycle.example.yaml` for a tour. |

## Adapters

- **TypeScript** — `npm install @loomcycle/client` → see `adapters/ts/`. HTTP+SSE. Used by `jobs-search-agent`.
- **Python** — `pip install loomcycle` → see `adapters/python/`. Async over `grpc.aio`; covers all seven RPCs. Shipped in v0.5.5.

## Security

- **No vendor binary.** Pure HTTP to provider APIs; no subprocess auth inheritance vector (the class of bug that produced an $80 cost incident in early production).
- **Default-deny everything.** Tool-by-tool, agent-by-agent. New tools and new agents start with zero privilege.
- **Two-layer policy + per-request narrowing.** Operator gates tools at the env layer; agents narrow at the YAML layer; callers can shrink further per-run via `allowed_tools` and `allowed_hosts`. Caller can never widen.
- **SSRF defence in HTTP / WebFetch.** Hostname allowlist + RFC1918/loopback/link-local IP block at the dial layer. Defeats DNS rebinding.
- **Constant-time bearer auth.**
- **`Bash` is NOT a sandbox.** Enable only inside a container or VM. The tool restricts cwd, scrubs env, bounds output, and times out — but cannot prevent an enabled-but-malicious agent from reading absolute paths or making network calls.

## Documentation

- `docs/ARCHITECTURE.md` — request flow, provider abstraction, agent loop, sub-agents, skills, storage, concurrency, cancellation.
- `docs/TOOLS.md` — the two-layer default-deny model end-to-end, every built-in tool, MCP / LocalAPI integrations, per-request narrowing.
- `docs/POSTGRES.md` — operator guide for the v0.5.0 Postgres backend: configuration, migrations, sqlite→postgres data migration runbook, concurrency benchmark.
- `docs/GRPC.md` — operator guide for the v0.5.5 gRPC surface: enablement, wire-shape parity with HTTP+SSE, error mapping, TLS / coexistence recipes, Python adapter quick-start.
- `docs/PLAN.md` — public roadmap. v0.4.0 / v0.5.0 / v0.5.5 / v0.6.0 / v0.7.0 shipped status; v0.7.x near-term + v1.0 outline.
- `CLAUDE.md` — project guide for agents working in this repo (Claude Code).

## License

Apache-2.0. See [LICENSE](LICENSE).
