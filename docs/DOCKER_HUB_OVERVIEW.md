<!--
This file is the source of truth for the Docker Hub repository
overview at https://hub.docker.com/r/denngubsky/loomcycle.

Docker Hub doesn't auto-sync from a repo file; copy-paste this
into the "Overview" field on the Hub page when material changes
ship. Update on every minor / major version (patches usually
don't change the overview).

Markdown features supported by Docker Hub: standard CommonMark
(headings, lists, links, code blocks, tables). NO HTML.
-->

# loomcycle

**High-load agentic runtime — one Go binary that owns the LLM tool-use loop end-to-end, runs as a sidecar, multi-provider (Anthropic / OpenAI / DeepSeek / Gemini / Ollama), multi-tenant.**

`loomcycle` is the substrate underneath your agents: it talks model APIs, runs the tool-use loop, manages concurrency + retries + audit, and exposes a small HTTP+SSE surface that any application server can consume. No vendor SDK in the loop. No subprocess auth inheritance. Pure HTTP to provider APIs.

## Quick start

```bash
# Pull the latest stable
docker pull denngubsky/loomcycle:latest

# Write a default config into a mounted directory
mkdir -p ./config ./data
docker run --rm -v $(pwd)/config:/home/nonroot/.config/loomcycle \
  denngubsky/loomcycle:latest init --no-interactive

# Run with required env vars
docker run -d --name loomcycle \
  -p 127.0.0.1:8787:8787 \
  -v $(pwd)/config:/home/nonroot/.config/loomcycle:ro \
  -v $(pwd)/data:/home/nonroot/.local/share/loomcycle \
  -e LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32) \
  -e ANTHROPIC_API_KEY=your-key-here \
  -e LOOMCYCLE_LISTEN_ADDR=0.0.0.0:8787 \
  denngubsky/loomcycle:latest

# Verify
docker exec loomcycle /usr/local/bin/loomcycle doctor || true
curl http://127.0.0.1:8787/healthz
```

## Tags

| Tag | What it points at |
|---|---|
| `latest` | Most recent stable release |
| `vX.Y.Z` | Exact pin (recommended for production) |

No `vX` or `vX.Y` floating tags during the v0.11.x line — too early for major-version stability promises. Pin against `vX.Y.Z` in production; use `latest` for development.

## Configuration

### Required environment variables

| Variable | Purpose | How to get it |
|---|---|---|
| `LOOMCYCLE_AUTH_TOKEN` | Bearer for every `/v1/*` endpoint | `openssl rand -hex 32` |
| `<PROVIDER>_API_KEY` | At least one provider key | `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `DEEPSEEK_API_KEY` / `GEMINI_API_KEY` |

### Common optional environment variables

| Variable | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_LISTEN_ADDR` | `127.0.0.1:8787` | Set to `0.0.0.0:8787` inside the container so port-mapping works |
| `LOOMCYCLE_STORAGE_BACKEND` | `sqlite` | Set to `postgres` to use the Postgres backend |
| `LOOMCYCLE_PG_DSN` | (unset) | Postgres DSN — required when backend is postgres |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` | (unset) | OTLP endpoint for distributed traces; unset = tracing off |

### Volume mounts

| Container path | What it's for | Mode |
|---|---|---|
| `/home/nonroot/.config/loomcycle` | yaml config + per-machine `README.md` | typically read-only |
| `/home/nonroot/.local/share/loomcycle` | SQLite store + transient state | writable |

On Linux hosts, the writable mount must be owned by uid 65532:

```bash
mkdir -p ./data && sudo chown -R 65532:65532 ./data
```

Docker Desktop on macOS handles this transparently via its file-sharing layer.

## Docker Compose

A ready-to-use example lives at [`docker-compose.example.yaml`](https://github.com/denn-gubsky/loomcycle/blob/main/docker-compose.example.yaml) in the repo. Copy + customise:

```bash
curl -O https://raw.githubusercontent.com/denn-gubsky/loomcycle/main/docker-compose.example.yaml
mv docker-compose.example.yaml docker-compose.yaml
# Edit docker-compose.yaml + create .env with required vars
docker compose up -d
```

The compose file documents the Postgres backend upgrade path (commented out by default — uncomment when you need multi-replica HA or a more robust storage backend).

## Image details

- **Base:** `gcr.io/distroless/static:nonroot` — no shell, no package manager, runs as uid 65532.
- **Size:** ~10 MB compressed, ~40 MB on disk.
- **Architectures:** `linux/amd64` + `linux/arm64` (Apple Silicon under Docker Desktop pulls arm64 automatically).
- **Build:** pure Go static binary (CGO_ENABLED=0), reproducible builds.
- **No `docker exec ... sh`** — distroless has no shell. Use `docker logs` to debug.
- **OCI labels** identify source, version, commit SHA, and Apache-2.0 license for supply-chain inspectors.

## What you get inside

- **`POST /v1/runs`** — run an agent with SSE event stream (tool-use loop, native cache_control on Anthropic, retries with backoff).
- **`POST /v1/_llm/chat`** (v0.11.0+) — direct LLM gateway bypassing the agent loop. LangChain-friendly request / Anthropic-style response. Use this from n8n's AI Agent Chat Model slot or any LangChain `BaseChatModel`.
- **Web UI at `/ui`** — admin console for runs, library (agents / skills / MCP servers), memory, snapshots, channels.
- **Built-in tools** (default-deny; enable via env vars): Read, Write, Edit, Grep, Glob, NotebookEdit, HTTP, WebFetch, WebSearch, Bash, Agent (sub-agents), Skill, Memory, Channel, AgentDef, SkillDef, MCPServerDef, Evaluation, Interruption, Context.
- **MCP integration** — stdio pool + Streamable HTTP. Mount MCP server tools into any agent's `allowed_tools`.
- **Resolver matrix** — multi-provider routing with per-user tier overlays, cascading fallback on retryable errors.

## First-run flow

The v0.11.1 `init` + `doctor` commands make setup straightforward:

```bash
# Inside the running container — or use the host docker run pattern above
loomcycle init       # bootstrap loomcycle.yaml + README.md in ~/.config/loomcycle/
loomcycle doctor     # verify config + env vars + providers + storage + listen
loomcycle            # start the server (default behaviour with no args)
```

`loomcycle doctor` exits 0 when everything is green; 1 on any FAIL. Operators running in CI should script around the exit code.

## Documentation

- **GitHub repo + full README:** https://github.com/denn-gubsky/loomcycle
- **Architecture:** https://github.com/denn-gubsky/loomcycle/blob/main/docs/ARCHITECTURE.md
- **Tools + security model:** https://github.com/denn-gubsky/loomcycle/blob/main/docs/TOOLS.md
- **Provider routing + tiers:** https://github.com/denn-gubsky/loomcycle/blob/main/docs/CONFIGURATION.md
- **MCP integration:** https://github.com/denn-gubsky/loomcycle/blob/main/docs/MCP_INTEGRATION.md
- **Postgres backend:** https://github.com/denn-gubsky/loomcycle/blob/main/docs/POSTGRES.md
- **gRPC surface:** https://github.com/denn-gubsky/loomcycle/blob/main/docs/GRPC.md
- **Release notes:** https://github.com/denn-gubsky/loomcycle/blob/main/REVISIONS.md

In-binary docs (bundled help topics) are accessible via `GET /v1/_help/<topic>` against a running instance. Topic list: `installation`, `getting-started`, `llm-gateway`, `fairness`, `observability`, `vector-memory`, `voyage-embedder`, `sqlite-vec`, `dynamic-mcp`, `bash-security`, and more.

## Adapters

- **TypeScript:** `npm install @loomcycle/client` (HTTP+SSE, 41 methods, dual ESM+CJS)
- **Python:** `pip install loomcycle` (gRPC async)

## Source + license

- **Repository:** https://github.com/denn-gubsky/loomcycle (Apache-2.0)
- **License:** Apache-2.0
- **Issues / discussions:** https://github.com/denn-gubsky/loomcycle/issues

## Registry note

Docker Hub strips hyphens from usernames. The GitHub org `denn-gubsky` becomes `denngubsky` on Hub — pin against `denngubsky/loomcycle`, not the hyphenated form. (The Homebrew tap at `denn-gubsky/homebrew-loomcycle` keeps the hyphen.)
