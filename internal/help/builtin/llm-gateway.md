---
name: llm-gateway
description: "POST /v1/_llm/chat — direct provider routing without the agent loop. For n8n LoomCycleChatModel + LangChain-compatible consumers."
---
Loomcycle v0.11.0 ships an LLM Gateway endpoint that exposes the
resolver + provider auth + retry layer directly, bypassing the agent
loop. Use it when you only need provider routing (no tools, no memory,
no agent semantics) — n8n's AI Agent Chat Model slot is the immediate
target, but any LangChain-compatible consumer can hit it.

## When to use the gateway vs an agent

Pick **the gateway** (`POST /v1/_llm/chat`) when:

- You're powering an external orchestrator's reasoning turns (n8n AI
  Agent, LangChain, custom workflow engine, IDE assistant).
- You want loomcycle's routing benefits — one credential, one
  per-user quota, one observability surface across providers — without
  paying the ~50-200 ms per-turn overhead of a full agent run.
- The "agent" you'd otherwise declare would have `system_prompt: ""` +
  `allowed_tools: []` — i.e. a passthrough shell with no actual loop
  work.

Pick **an agent run** (`POST /v1/runs`) when:

- You need tools (built-in or MCP), memory, snapshots, hooks, or
  audit-event persistence.
- The "agent" has real loop semantics (system prompt, allowed tools,
  iteration limits).
- You want runs to appear in `/ui/runs` with the full transcript +
  cancel API + the v0.9.x state stream.

The two are complementary — same provider, same auth, same routing,
different wire surface.

## Wire shape

Request (POST /v1/_llm/chat, bearer-authed):

```json
{
  "messages": [
    { "role": "system", "content": "You are helpful." },
    { "role": "user", "content": "What is 2+2?" }
  ],
  "tools": [
    {
      "name": "calculator",
      "description": "Evaluate math",
      "input_schema": { "type": "object", "properties": { "expr": { "type": "string" } } }
    }
  ],
  "max_tokens": 4096,
  "temperature": null,
  "stream": false,
  "provider": null,
  "model": null,
  "tier": "default",
  "user_id": "alice",
  "user_tier": "pro"
}
```

Non-streaming response:

```json
{
  "id": "llm_abc",
  "request_id": "req_xyz",
  "provider": "anthropic",
  "model": "claude-sonnet-4-6",
  "content": [
    { "type": "text", "text": "5 * 7 = 35" }
  ],
  "stop_reason": "end_turn",
  "usage": { "input_tokens": 1234, "output_tokens": 56 }
}
```

Streaming response (`stream: true`) — Anthropic-style event names:

```
event: provider_chosen
data: {"provider":"anthropic","model":"claude-sonnet-4-6","request_id":"req_..."}

event: content_block_start
data: {"index":0,"block":{"type":"text","text":""}}

event: content_block_delta
data: {"index":0,"delta":{"type":"text_delta","text":"Sure! "}}

event: content_block_stop
data: {"index":0}

event: message_delta
data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":56}}

event: done
data: {"id":"llm_...","stop_reason":"end_turn","usage":{...}}
```

On terminal failure (auth, provider down, retry exhausted):

```
event: error
data: {"type":"provider_error","code":"provider_call_failed","message":"..."}
```

## Routing precedence

When the request includes routing hints, the gateway applies them in
this order (highest priority first):

1. **`provider` AND `model` both set** — explicit pin; the resolver
   short-circuits and trusts the operator's choice. Useful when you
   want full control and the consumer already knows which model.
2. **`provider` only set** — resolver picks the best model within
   that provider given the `tier` / `user_tier` policy.
3. **`model` only set** — resolver picks the provider hosting that
   model (matched against the candidate list).
4. **Neither set** — full resolver pick by `tier` / `user_tier`.
   Defaults to `tier: "default"` when no hint supplied.

The chosen `provider` + `model` are echoed back on the non-streaming
response and on the `provider_chosen` SSE frame for streaming — so
consumers can log / display the routing decision.

## Tool calling

The gateway accepts the provider-agnostic tool schema
(`{name, description, input_schema}`) and forwards it to the chosen
driver, which translates to the provider's native format internally:

- **Anthropic:** pass-through (`input_schema` already matches).
- **OpenAI:** wrapped in `{type:"function", function:{name, description, parameters:input_schema}}`.
- **DeepSeek:** same as OpenAI (compatible API).
- **Gemini:** `tools[].function_declarations[]` (with `$ref`/`oneOf`
  inlining via the v0.8.10 sanitizer).
- **Ollama:** function-style for models that support tools; refusal
  warning otherwise.

When the model returns a tool call, the response's `content` array
contains a `tool_use` block. Provide the result by appending a
`role: "tool"` message with `tool_call_id` matching the original
`tool_use.id` and `content` carrying the result text — the gateway
translates that back to each provider's tool-result message shape.

## Authentication

Bearer-authed admin endpoint — same `LOOMCYCLE_AUTH_TOKEN` as every
`/v1/_*` route. The gateway is operator-trust scope (n8n workflows,
internal services); end users never hit it directly.

When `user_id` is in the request, the existing per-user concurrency
quota applies — see `help fairness` for the policy. Anonymous calls
(empty `user_id`) bypass the per-user cap but still count against the
global semaphore.

## Audit + observability (v0.11.0 posture)

Every gateway request logs a structured line at completion:

```
llm_gateway: request_id=req_abc provider=anthropic model=claude-sonnet-4-6 \
  tier="default" user_id="alice" input_tokens=1234 output_tokens=56 \
  stop_reason=end_turn latency_ms=842 status=ok err=""
```

Scrape these via stdout / `journalctl` / a log shipper. v0.11.1
follow-up adds a dedicated `gateway_events` table queryable via a new
HTTP endpoint — gateway calls are too high-cardinality (n8n workflows
fire dozens per execution) to share the `events` table with agent
runs.

When OTEL is configured (see `help observability`), each gateway call
emits a `loomcycle.provider.call` span with the standard provider /
model / tier / user_id / token-count attributes — the same shape agent
runs emit, so existing dashboards continue to work.

## TS adapter usage

`@loomcycle/client@0.11.0` ships `llmChat()` + `llmStream()`:

```typescript
import { LoomcycleClient } from "@loomcycle/client";

const client = new LoomcycleClient({
  baseUrl: "http://localhost:8787",
  authToken: process.env.LOOMCYCLE_AUTH_TOKEN,
});

// Non-streaming
const resp = await client.llmChat({
  messages: [{ role: "user", content: "What's 2+2?" }],
  max_tokens: 100,
  tier: "default",
});

// Streaming
for await (const frame of client.llmStream({
  messages: [{ role: "user", content: "Count to 3" }],
  stream: true,
})) {
  if (frame.kind === "content_block_delta") {
    process.stdout.write(frame.payload.delta.text ?? "");
  }
}
```

## Comparison with LiteLLM / Portkey

| | LiteLLM | Portkey | loomcycle gateway |
|---|---|---|---|
| Multi-provider routing | yes | yes | yes (resolver matrix) |
| Per-user quotas | no | yes (paid) | yes (built-in) |
| Tier-based dispatch | no | yes (paid) | yes |
| OTEL spans | partial | yes | yes (v0.10.0) |
| Substrate (agents/tools/memory) | no | no | yes (same binary) |
| Wire format | OpenAI-compat | OpenAI-compat | Anthropic-style |
| OpenAI-compat shim | yes | yes | deferred to v0.11.1+ |

The gateway is one of two products loomcycle bundles in the same
binary — pick the gateway when you only need routing; use the agent
runtime when you need the full loop.

## Related topics

- `fairness` — per-user concurrency quotas + tier policy.
- `observability` — OTEL setup for span-based gateway observability.
- `n8n-comparison` — n8n integration story; LoomCycleChatModel sub-node.
