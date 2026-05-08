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

## What it is

LoomCycle is a single Go binary that runs as a local sidecar and serves an HTTP+SSE API to your application. It owns the model→tool_use→tool_result→model loop, talks directly to provider HTTP APIs (no vendor SDK in the loop, no bundled binary), and dispatches tool calls to built-ins, MCP servers, or operator-supplied OpenAPI gateways. Multi-tenant. Multi-provider. Multi-agent (parents spawn sub-agents).

It exists to replace bundled-binary agent SDKs that cold-start in 20–30 s, leak memory under load, and lock you into one provider — the things that made the first production user (`jobs-search-agent`) infeasible to scale on a small VPS.

## Why this approach

- **Pure HTTP loop.** No vendor binary spawned per call. The runtime is one Go process, ~16 MB compiled, single static binary. Cold-start is the kernel's exec time.
- **Provider-agnostic.** Four drivers — Anthropic Messages, OpenAI Chat Completions, DeepSeek (OpenAI-compatible), Ollama `/api/chat` — all normalize to one `Event` channel the loop drains. Capability flags expose provider-specific extras (Anthropic `cache_control`, OpenAI / DeepSeek parallel tool calls).
- **Per-agent provider routing.** YAML `provider:` field per agent lets a consumer mix backends by data sensitivity: Anthropic for user-sensitive paths, DeepSeek for high-volume public-data work, Ollama (local llama) for offline / cost-floor scenarios. Same wire surface, different cost / privacy posture per agent.
- **Default-deny tool policy.** Every built-in is disabled until env-configured. Every agent gets zero tools until `allowed_tools` is set in YAML. Two layers must say "yes" before a tool reaches the model.
- **Native cache placement.** When the provider supports it (Anthropic), system blocks marked `cacheable: true` carry `cache_control` on the wire — you keep cache reads on the stable preamble even when the rest of the conversation churns.
- **Two wire surfaces.** HTTP+SSE (default) and gRPC (opt-in via `LOOMCYCLE_GRPC_ADDR`). Both share the same store, cancel registry, runner, and concurrency semaphore — picking one is a wire-format decision, not a feature decision. A cancel issued via gRPC reaches a run started via HTTP and vice versa.
- **Observable everywhere.** Every text chunk, tool call, tool result, usage update, and retry is an SSE / gRPC event. Nothing happens silently.

## Quick start

```bash
# 1. Build
go build -o bin/loomcycle ./cmd/loomcycle

# 2. Configure
cp .env.example .env.local       # set ANTHROPIC_API_KEY etc.
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
```

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
- `docs/PLAN.md` — public roadmap. v0.4.0 / v0.5.0 / v0.5.5 / v0.6.0 shipped status; v0.7+ near-term + v1.0 outline.
- `CLAUDE.md` — project guide for agents working in this repo (Claude Code).

## License

Apache-2.0. See [LICENSE](LICENSE).
