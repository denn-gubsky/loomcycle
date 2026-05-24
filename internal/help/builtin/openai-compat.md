---
name: openai-compat
description: "POST /v1/chat/completions — drop-in OpenAI Chat Completions wire shape over loomcycle's LLM gateway."
---
Loomcycle v0.11.3 ships an OpenAI Chat Completions compatibility shim
at `POST /v1/chat/completions`. Same wire shape as OpenAI's hosted
API; consumers using the OpenAI SDK can point at loomcycle by
changing only the base URL + auth token.

Same dispatch path as the native gateway endpoint (`/v1/_llm/chat`):
resolver routing, per-user quota, audit logging — all in one place.
The shim is a pure wire-format translator. A bug in routing/quota/
retry shows up in both paths; a bug here is a translation bug.

## Why this exists

Every OpenAI-SDK tool out there (Aider, Goose, Continue, Cursor,
Cody, custom code, every "use OpenAI as your LLM" tutorial) hardcodes
the OpenAI URL + request shape. v0.11.0's native `/v1/_llm/chat`
gives consumers loomcycle's routing benefits but requires writing
loomcycle-specific client code. The shim closes that gap — point any
OpenAI client at loomcycle and it Just Works.

## Drop-in example (Python OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8787/v1",
    api_key="<your-LOOMCYCLE_AUTH_TOKEN>",
)

resp = client.chat.completions.create(
    model="claude-sonnet-4-6",            # any model the resolver knows
    messages=[{"role": "user", "content": "What is 2+2?"}],
    max_tokens=100,
)
print(resp.choices[0].message.content)
```

## Drop-in example (TypeScript OpenAI SDK)

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://127.0.0.1:8787/v1",
  apiKey: process.env.LOOMCYCLE_AUTH_TOKEN,
});

const stream = await client.chat.completions.create({
  model: "claude-sonnet-4-6",
  messages: [{ role: "user", content: "Count to 5." }],
  stream: true,
});

for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
}
```

## Routing — loomcycle extensions

Pass the standard OpenAI fields as usual. To use loomcycle's resolver
features (multi-provider routing, tier overlays, per-user quota), the
shim accepts these namespaced extension fields in the request body:

| Field | Purpose |
|---|---|
| `loomcycle_provider` | Pin to a specific provider (e.g. `"anthropic"`). When omitted, the resolver picks based on the `model` field. |
| `loomcycle_tier` | Tier for the resolver dispatch (`"low"` / `"middle"` / `"high"` / etc.). |
| `loomcycle_user_id` | Per-user quota tracking + audit log key. |
| `loomcycle_user_tier` | Per-user tier overlay; takes precedence over `loomcycle_tier`. |

The standard OpenAI `user` field is also mapped to `loomcycle_user_id`
automatically when the explicit field isn't set — so SDK callers
who already pass `user: "alice"` get per-user quota tracking for
free.

Example with extensions:

```python
resp = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "Hello"}],
    user="alice",                          # → loomcycle_user_id="alice"
    extra_body={
        "loomcycle_provider": "anthropic",
        "loomcycle_tier": "high",
    },
)
```

## What's translated

**Request (OpenAI → loomcycle):**

- `messages[].content` — flat string OR `[{type:"text", text:"..."}]` array; multimodal image/audio parts silently skipped in v1.
- `messages[].tool_calls[].function.arguments` — OpenAI passes args as a JSON STRING; loomcycle's native shape wants a parsed object. Shim parses it.
- `tools[]` — OpenAI's `{type:"function", function:{name, description, parameters}}` envelope unwrapped to loomcycle's flat `{name, description, input_schema}`.
- `model`, `messages`, `tools`, `max_tokens`, `temperature`, `stream` — pass-through.

**Response (loomcycle → OpenAI):**

- Native `content` array → `choices[0].message.content` (text blocks concatenated) + `choices[0].message.tool_calls` (tool_use blocks wrapped in OpenAI's function envelope).
- `stop_reason` → `finish_reason`: `end_turn` / `stop_sequence` → `stop`; `max_tokens` → `length`; `tool_use` → `tool_calls`.
- `usage` → `prompt_tokens` / `completion_tokens` / `total_tokens` shape.
- Streaming: `data: <json>` frames in the `chat.completion.chunk` shape, terminated by `data: [DONE]` (bare `data:` lines, NO named SSE events — OpenAI's protocol).

## Accepted-but-ignored fields

These OpenAI fields are accepted (so SDK consumers don't get
validation errors) but ignored — loomcycle doesn't apply them:

`n`, `presence_penalty`, `frequency_penalty`, `top_p`, `seed`,
`response_format`, `logit_bias`, `tool_choice`, `top_logprobs`,
`stop` (stop sequences).

If a future loomcycle release wires any of these into the providers
layer, the shim's translator picks them up automatically.

## Authentication

Same as every `/v1/*` endpoint: bearer-authed with
`LOOMCYCLE_AUTH_TOKEN`. The OpenAI SDKs' `api_key` parameter sets
the `Authorization: Bearer <key>` header automatically — pass your
loomcycle bearer there.

## Audit

Each request emits the same structured log line as the native
gateway:

```
llm_gateway: request_id=req_abc provider=anthropic model=claude-sonnet-4-6 \
  tier="" user_id="alice" input_tokens=1234 output_tokens=56 \
  stop_reason=end_turn latency_ms=842 status=ok err=""
```

OTEL spans (when configured) carry the same attributes — operators
graphing per-provider / per-user metrics see openai-compat calls
alongside native gateway calls under one observability surface.

## Related topics

- `llm-gateway` — native loomcycle gateway endpoint (richer wire
  shape: Anthropic-style content blocks + named SSE events).
- `installation` — install paths to get loomcycle running.
- `fairness` — per-user concurrency quota policy.
- `observability` — OTEL setup.
