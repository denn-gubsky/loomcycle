# loomcycle

A lightweight, multi-tenant agentic runtime — one Go sidecar that owns the LLM tool-use loop end-to-end across Anthropic, OpenAI and Ollama. Consumed from Next.js (TS adapter) and Python over a local HTTP+SSE API.

> **Status:** v0.1 in active development. Not production-ready. Anthropic provider works; OpenAI / Ollama / MCP / SQLite store / Python adapter / caching layers are scaffolded but not yet implemented.

## Why

Existing agent SDKs (`@anthropic-ai/claude-agent-sdk`, OpenAI Agents SDK) bundle a vendor binary, hide the tool-use loop, and lock you into one provider. For multi-tenant SaaS on a 4–8 GiB VPS that means: 20–30 s cold-start per agent, no control over native prompt cache placement, and no path to mix in a cheaper local model when one would do.

loomcycle owns the loop and stays small.

## Design (one paragraph)

A single Go binary serves an HTTP+SSE API. Each call enters a goroutine, acquires a slot from a global semaphore (default 8 concurrent), runs the model→tool_use→tool_result loop against a chosen provider, dispatches tool calls to built-ins or MCP servers (stdio pool + HTTP), persists transcript + usage to the configured `Store`, and streams every event to the caller. Per-tenant fairness, per-agent tool allowlists, and provider-native prompt caching (where supported) are first-class.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Quick start

```bash
# 1. Build
go build -o bin/loomcycle ./cmd/loomcycle

# 2. Configure
cp .env.example .env       # then edit ANTHROPIC_API_KEY etc.
cp loomcycle.example.yaml loomcycle.yaml

# 3. Run
./bin/loomcycle --config loomcycle.yaml

# 4. Call (from another terminal)
curl -N http://127.0.0.1:8787/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "agent": "default",
    "segments": [{"role":"user","content":[{"type":"trusted-text","text":"Hello"}]}]
  }'
```

## Adapters

- **TypeScript** — `npm install @loomcycle/client` → see `adapters/ts/`.
- **Python** — `pip install loomcycle` → see `adapters/python/`.

## License

Apache-2.0. See [LICENSE](LICENSE).
