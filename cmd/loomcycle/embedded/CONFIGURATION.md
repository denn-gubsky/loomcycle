# loomcycle — operator configuration reference

This file ships next to `loomcycle.yaml` whenever you run `loomcycle init`. It covers the conceptual model + environment variable reference; the yaml itself carries heavily-commented per-section documentation, so read both.

## Where loomcycle looks for config

When you start `loomcycle` without `--config`, the binary auto-discovers the first file that exists in this order:

1. `./loomcycle.yaml` (current working directory)
2. `$XDG_CONFIG_HOME/loomcycle/loomcycle.yaml` (typically `~/.config/loomcycle/loomcycle.yaml`)
3. `~/.config/loomcycle/loomcycle.yaml` (fallback when `$XDG_CONFIG_HOME` is unset)

If you pass `--config /path/to/your.yaml` explicitly, auto-discovery is bypassed entirely.

## File layout

The default install (after `loomcycle init`) lays out:

```
~/.config/loomcycle/
├── loomcycle.yaml          ← your config; edit this
├── CONFIGURATION.md         ← this file
└── (you may add)            extra files: agents/, skills/, hooks/
~/.local/share/loomcycle/
└── loomcycle.db             ← default sqlite store (override with LOOMCYCLE_DATA_DIR)
```

## Required environment variables

`loomcycle init` does NOT write secrets to disk. Set these in your shell rc (`~/.zshrc` / `~/.bashrc` / etc.):

| Env var | Purpose | How to get it |
|---|---|---|
| `LOOMCYCLE_AUTH_TOKEN` | Bearer token for all `/v1/*` endpoints | `openssl rand -hex 32` |

Plus at least one provider key (whichever provider(s) your yaml routes to):

| Env var | Provider | Where to get it |
|---|---|---|
| `ANTHROPIC_API_KEY` | Anthropic Claude | console.anthropic.com |
| `OPENAI_API_KEY` | OpenAI | platform.openai.com |
| `DEEPSEEK_API_KEY` | DeepSeek | platform.deepseek.com |
| `VOYAGE_API_KEY` | Voyage AI (embeddings) | dash.voyageai.com |
| `OLLAMA_API_KEY` | Ollama Cloud (hosted models) | ollama.com |

Local Ollama (`ollama-local` provider) needs no key — it reads `OLLAMA_BASE_URL` (default `http://127.0.0.1:11434`).

## Optional environment variables

| Env var | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_DATA_DIR` | `~/.local/share/loomcycle` | Where sqlite + transient state live |
| `LOOMCYCLE_STORAGE_BACKEND` | `sqlite` | Set to `postgres` to enable the Postgres backend |
| `LOOMCYCLE_PG_DSN` | (empty) | Postgres DSN — required when backend is postgres |
| `LOOMCYCLE_PG_AUTOMIGRATE` | `false` | Auto-run schema migrations on boot |
| `LOOMCYCLE_PGVECTOR_ENABLED` | `false` | Enable pgvector for Vector Memory (Postgres only) |
| `LOOMCYCLE_OTEL_EXPORTER_OTLP_ENDPOINT` | (empty) | OTLP endpoint for distributed traces; empty = tracing off |
| `LOOMCYCLE_BASH_ENABLED` | `false` | Enable the Bash tool (NOT a sandbox — see `help bash-security`) |
| `LOOMCYCLE_AGENTS_ROOT` | (empty) | Directory of `.md` agent definitions; merged with yaml `agents:` block |
| `LOOMCYCLE_SKILLS_ROOT` | (empty) | Directory of skills; merged with bundled skills |
| `LOOMCYCLE_HELP_ROOT` | (empty) | Directory of help topics; merged with bundled topics |

## YAML structure (high level)

The bundled `loomcycle.yaml` walks every section in detail. Quick conceptual map:

- **`providers:`** — list of provider IDs the resolver may pick from (`anthropic`, `openai`, `deepseek`, `gemini`, `ollama`, `ollama-local`).
- **`models:`** — per-tier candidate matrix. The resolver picks the first reachable candidate in each tier (`low` / `middle` / `high`).
- **`user_tiers:`** — per-user-tier policy overlays. Defines provider priority + fallback behaviour per user class (free / pro / etc.).
- **`agents:`** — named agent definitions. Each carries `model:` (or `tier:`), `system_prompt:`, `allowed_tools:`, `skills:`, `memory_scopes:`, etc.
- **`channels:`** — declared inter-agent pub/sub channels. ACL is operator-controlled here.
- **`mcp_servers:`** — declared MCP servers (HTTP / Streamable HTTP / stdio). Tools become callable from any agent's `allowed_tools:`.
- **`storage:`** — backend selection + connection settings. SQLite by default; Postgres opt-in.
- **`concurrency:`** — `max_concurrent_runs`, `max_queue_depth`, `queue_timeout_ms`, optional `max_concurrent_runs_per_user`.
- **`memory:`** — Memory tool defaults, Vector Memory embedder selection.

## In-binary help topics

Loomcycle bundles a `Context.help` registry that agents (and operators via `GET /v1/_help/<topic>`) can read. Topics relevant to first-run + ops:

- `getting-started` — this walkthrough from the agent's perspective
- `llm-gateway` — direct LLM routing endpoint (v0.11.0)
- `fairness` — per-tenant concurrency quota policy
- `observability` — OTEL setup
- `sqlite-vec` — SQLite Vector Memory build-tag opt-in
- `voyage-embedder` — Anthropic-blessed embedder via Voyage AI
- `dynamic-mcp` — register MCP servers at runtime
- `skills-evolution` — agents writing their own skills

Read them via:

```sh
loomcycle  # in one shell
# in another:
curl -sH "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  http://127.0.0.1:8787/v1/_help/getting-started
```

## Troubleshooting

When something doesn't work, run:

```sh
loomcycle doctor
```

It walks 6 checks (config present + parses, auth token set, per-provider key + probe, storage writable, listen address bindable) and prints PASS/WARN/FAIL per check with one-line guidance. Most first-run problems surface there.

Specific symptoms:

- **`failed to load config`** — run `loomcycle init` to create the default tree, or pass `--config /path/to/yaml`.
- **`401 unauthorized` on every endpoint** — `LOOMCYCLE_AUTH_TOKEN` is set on the server but the client isn't sending it. Use the same value in `Authorization: Bearer <token>`.
- **Provider returns 401 / 403** — the per-provider API key is missing or wrong. Check `loomcycle doctor` for which key the resolver expects.
- **`vector_unsupported` from Memory tool** — Vector Memory needs pgvector (Postgres + `LOOMCYCLE_PGVECTOR_ENABLED=1`) OR the sqlite-vec build (`go install -tags=sqlite_vec ...`). See `help sqlite-vec`.

## Next steps after first install

1. Run `loomcycle doctor` and resolve any FAILs.
2. Read `loomcycle.yaml` top-to-bottom — every section is commented.
3. Pick which provider tier to dispatch your default agent to (edit `agents.default.tier:` or `agents.default.model:`).
4. Run `loomcycle` to start the server on `127.0.0.1:8787`.
5. Open `http://127.0.0.1:8787/ui?token=$LOOMCYCLE_AUTH_TOKEN` for the Web UI.
