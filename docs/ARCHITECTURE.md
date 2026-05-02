# Architecture

## What this is

`loomcycle` is a single Go binary that:
1. Owns the LLM **tool-use loop** (model → tool_use → tool_result → model) end-to-end. No vendor SDK in the loop, no bundled binary.
2. Talks to one or more **providers** (Anthropic Messages today; OpenAI / Ollama next) over their HTTP APIs.
3. Dispatches tool calls to **built-in tools** or **MCP servers** (stdio pool + HTTP — both deferred from the v0.1 thin slice).
4. Streams every event back to callers as **SSE** over a small HTTP API.
5. Caps concurrency with a **semaphore + bounded queue** to keep memory predictable on a small VPS.

## Slice landed in this commit

```
cmd/loomcycle           ← sidecar binary
internal/providers      ← Provider interface + Capabilities
internal/providers/anthropic  ← Anthropic Messages API streaming + cache_control
internal/tools          ← Tool + Dispatcher
internal/tools/builtin  ← Read (the only built-in for now)
internal/tools/policy   ← per-agent allow/deny + glob
internal/loop           ← the agent loop
internal/concurrency    ← semaphore + queue + backpressure
internal/config         ← YAML + .env loader
internal/api/http       ← POST /v1/runs (SSE) + /healthz + bearer auth
adapters/ts             ← TypeScript client (typed AsyncIterable<AgentEvent>)
```

## Request flow (POST /v1/runs)

```
HTTP POST /v1/runs
  │
  ▼
authMiddleware  (LOOMCYCLE_AUTH_TOKEN check)
  │
  ▼
config.ResolveAgentModel(agent)  → (provider, model)
  │
  ▼
providerResolver.Get(provider)
  │
  ▼
sem.Acquire(ctx)  → 429 if backpressure
  │
  ▼
policy.Apply(available, agent.allowed, caller.allowed)
  │
  ▼
loop.Run(provider, dispatcher, segments, onEvent)
  │   ┌──────────────────────────────┐
  │   │ for iter := 0..MaxIterations │
  │   │   provider.Call(req) → events│
  │   │   collect text + tool_use    │
  │   │   if !tool_use: break        │
  │   │   for each tool_use:         │
  │   │     dispatcher.Execute(...)  │
  │   │   append tool_result message │
  │   └──────────────────────────────┘
  │
  ▼
sse.send(event) per emit  (text-event-stream)
```

## Provider interface

```go
type Provider interface {
    ID() string
    Capabilities() Capabilities
    Call(ctx context.Context, req Request) (<-chan Event, error)
}
```

The driver streams typed `Event`s to the loop. The loop is provider-agnostic;
adding OpenAI or Ollama means writing a driver that translates each provider's
streaming format into the same `Event` channel — nothing else changes.

`Capabilities` lets the loop decline to set fields the provider doesn't honour
(e.g. only Anthropic gets `cache_control`).

## Cache control (Anthropic only)

`PromptContentBlock.Cacheable: true` (caller side) → `Cacheable: true` on
`providers.ContentBlock` (loop side) → `cache_control: {"type":"ephemeral"}`
on the wire (Anthropic driver). The driver places it exactly where the loop
asked, so we capture cache reads on the stable system preamble even when the
rest of the conversation churns.

## Untrusted-block wrapping

Caller content of type `untrusted-block` is wrapped in `<kind>…</kind>` tags
inside the user message before the request leaves the loop. Combined with a
system-prompt instruction (the agent's `system_prompt` in YAML), this gives
the model a clear signal to treat the wrapped content as data, not
instructions. This mirrors the pattern in `jobs-search-agent`'s
prompt-injection defense layer.

## Concurrency

Single global `Semaphore` with `MaxConcurrentRuns` slots and a FIFO waiter
queue (`MaxQueueDepth` deep, `QueueTimeoutMS` per acquire). Acquired before
any SSE writes start, so backpressure surfaces as `HTTP 429` (with
`code:"backpressure"`) instead of mysterious mid-stream disconnects.

Per-tenant fairness is **not** in v0.1 — every caller competes for the same
pool. Adding a per-tenant token bucket on top is the obvious v0.2 next step.

## Storage (SQLite default)

The `Store` interface is defined in the plan but not wired in this thin slice.
Sessions are ephemeral: the HTTP request *is* the session. The next slice adds
SQLite-backed transcripts, then a `POST /v1/sessions/{id}/messages` endpoint
that continues an existing session.

## What is intentionally NOT in this slice

| Area | Status | Notes |
|---|---|---|
| OpenAI provider | not started | Driver plug-in, same `Provider` interface |
| Ollama provider | not started | Same |
| MCP stdio pool | not started | `internal/tools/mcp/stdio/` empty |
| MCP HTTP client | not started | `internal/tools/mcp/http/` empty |
| Sub-agents | not started | Anthropic-only at v0.2 |
| Response KV cache | not started | `internal/cache/response/` empty |
| SQLite Store | not started | `internal/store/sqlite/` empty |
| Postgres / Redis | v0.2 | Pluggable behind `Store` |
| Python adapter | not started | `adapters/python/` skeleton dir only |
| Built-in Write/Edit/Bash/HTTP/WebFetch | not started | Only `Read` for now |
| Conversation summarization | not started | v0.2 |
| Hooks (PreToolUse/PostToolUse) | not started | v0.2 |
| OpenTelemetry / metrics | not started | `internal/observability/` empty |

The architecture is shaped to absorb each of these without breaking the
`Provider`, `Tool`, `Store`, `Runner` interfaces — that's the point of getting
the seams right in the first slice.

## Verifying the slice

```bash
go test ./...           # all green
go build -o bin/loomcycle ./cmd/loomcycle
./bin/loomcycle --config loomcycle.example.yaml
# in another terminal:
curl http://127.0.0.1:8787/healthz
# {"ok":true}
```

For an end-to-end smoke against real Anthropic, set `ANTHROPIC_API_KEY` and
`LOOMCYCLE_AUTH_TOKEN` in `.env`, then POST to `/v1/runs`. (Caveat: the example
agent calls `claude-opus-4-7` which is paid; cheaper to drop a test agent
into your local YAML pointing at `claude-haiku-4-5`.)
