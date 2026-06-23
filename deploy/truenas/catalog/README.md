# loomcycle

A high-load agentic runtime — one Go binary that owns the LLM tool-use loop
end-to-end (model → tool_use → tool_result → model), runs as a sidecar, and is
consumed over HTTP+SSE / gRPC / MCP. Multi-provider (Anthropic, OpenAI, DeepSeek,
Gemini, Ollama), multi-tenant, multi-agent. Apache-2.0.

This is the TrueNAS SCALE **catalog app** source (Electric Eel 24.10+). The
provider/tier matrix is supplied by the binary's embedded presets (RFC AQ) — the
install form picks presets, the secrets, your Postgres DSN, the port, and the
dataset mounts. Secrets are TrueNAS-managed env; the app bundles no database
(point it at your existing Postgres 16). Ingress/TLS is the operator's existing
tunnel/proxy.

See [`../../../docs/TRUENAS.md`](../../../docs/TRUENAS.md) for the install/edit/
upgrade runbook, and [`../docker-compose.yaml`](../docker-compose.yaml) for the
no-wizard custom-app paste route.
