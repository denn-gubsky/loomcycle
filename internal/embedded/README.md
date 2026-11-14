# loomcycle — local config quickstart

This file ships next to `loomcycle.yaml` whenever you run `loomcycle init`. It's your **per-machine quickstart**: file layout, required env vars, troubleshooting. Read this first.

## What `init` wrote

`loomcycle init` writes the short config of a **profile**. Its one active line names a **preset**, a stack of config built into the binary that supplies the agents and the model routing:

```yaml
presets: [personal]     # `loomcycle init --profile server` writes [server]
```

Everything else in the file is yours (a volume, a local-model setting) and starts commented out. The line is read from this file only, your primary config; in any other config file it is an error. `LOOMCYCLE_PRESETS` (or `--preset`), when set, selects instead, and the server says so at start if the two differ. A preset named here is trusted as this file is: it is yaml, and can carry agents, MCP servers or sign-in clients. It cannot enable Bash, web access or any other switch that exists only in the environment. Without a preset the server starts with no agents, and `loomcycle doctor` says one is missing.

| Preset | Stands for | Use |
|---|---|---|
| `personal` | `base`, `local`, `chat`, `document-agent` | One person on one machine: local models first, cloud models when a key is set; SQLite. |
| `server` | `base`, `chat`, `document-agent`, `memory`, and the metrics sampler | A shared installation. It also needs a listen address, a public URL, secrets, volumes and sign-in gates: the repo's `examples/profiles/server/` has the env files and a compose file. |

`loomcycle presets` lists every preset and bundle, and `loomcycle presets show <name>` prints one with the stack it expands to. Anything you write in `loomcycle.yaml` is merged over the preset, key by key, so you restate a setting only to change it.

`loomcycle init --full` writes the long, fully-commented reference config instead. It carries its own routing and example agents and names no preset.

For the deeper conceptual story (provider routing matrix, tier overlays, the four-layer resolver), see the repo's [`docs/CONFIGURATION.md`](https://github.com/denn-gubsky/loomcycle/blob/main/docs/CONFIGURATION.md).

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
├── README.md                ← this file (per-machine quickstart)
└── (you may add)            extra files: agents/, skills/, hooks/
./data/                      ← relative to the directory you start loomcycle in
└── loomcycle.db             ← default sqlite store (set LOOMCYCLE_DATA_DIR to an absolute path for one fixed place)
```

## Required environment variables

`loomcycle init` writes no secret to disk, with one exception: `loomcycle init --with-token` generates `LOOMCYCLE_AUTH_TOKEN` and writes it to `auth.env` (mode `0600`) beside `loomcycle.yaml`, where `loomcycle` loads it. Set the rest in your shell rc (`~/.zshrc` / `~/.bashrc` / etc.), or add them to that file:

| Env var | Purpose | How to get it |
|---|---|---|
| `LOOMCYCLE_AUTH_TOKEN` | Bearer token for all `/v1/*` endpoints. Optional while the server listens on loopback only; required anywhere else | `openssl rand -hex 32` |

Plus at least one provider key, unless a local Ollama serves the models. A provider with no key is skipped:

| Env var | Provider | Where to get it |
|---|---|---|
| `ANTHROPIC_API_KEY` | Anthropic Claude | console.anthropic.com |
| `OPENAI_API_KEY` | OpenAI | platform.openai.com |
| `DEEPSEEK_API_KEY` | DeepSeek | platform.deepseek.com |
| `GEMINI_API_KEY` | Google Gemini | aistudio.google.com |
| `VOYAGE_API_KEY` | Voyage AI (embeddings) | dash.voyageai.com |
| `OLLAMA_API_KEY` | Ollama Cloud (hosted models) | ollama.com |

Local Ollama (`ollama-local` provider) needs no key — it reads `OLLAMA_BASE_URL` (default `http://127.0.0.1:11434`).

## Optional environment variables

| Env var | Default | Purpose |
|---|---|---|
| `LOOMCYCLE_DATA_DIR` | `./data`, relative to the directory you start `loomcycle` in | Where sqlite + transient state live |
| `LOOMCYCLE_PG_DSN` | (empty) | Postgres DSN. Set, the store is Postgres; empty, SQLite under the data dir |
| `LOOMCYCLE_PG_AUTOMIGRATE` | `0` | `1` runs schema migrations on boot; `0` does not. Any other value stops the load |
| `LOOMCYCLE_PGVECTOR_ENABLED` | `false` | Enable pgvector for Vector Memory (Postgres only) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (empty) | OTLP endpoint URL for distributed traces (standard `OTEL_*` set); empty = tracing off |
| `LOOMCYCLE_BASH_ENABLED` | `false` | Enable the Bash tool (NOT a sandbox — see `help bashbox` for the sandboxed alternative) |
| `LOOMCYCLE_AGENTS_ROOT` | (empty) | Directory of `.md` agent definitions; merged with yaml `agents:` block |
| `LOOMCYCLE_SKILLS_ROOT` | (empty) | Directory of skills; merged with bundled skills |
| `LOOMCYCLE_HELP_ROOT` | (empty) | Directory of help topics; merged with bundled topics |

## YAML structure (high level)

The reference config (`loomcycle init --full`) walks every section in detail; a profile's short config sets a few of these over its preset. Quick conceptual map:

- **`providers:`** — list of provider IDs the resolver may pick from (`anthropic`, `openai`, `deepseek`, `gemini`, `ollama`, `ollama-local`).
- **`models:`** — per-tier candidate matrix. The resolver picks the first reachable candidate in each tier (`low` / `middle` / `high`).
- **`user_tiers:`** — per-user-tier policy overlays. Defines provider priority + fallback behaviour per user class (free / pro / etc.).
- **`agents:`** — named agent definitions. Each carries `model:` (or `tier:`), `system_prompt:`, `tools:`, `skills:`, `memory_scopes:`, etc.
- **`channels:`** — declared inter-agent pub/sub channels. ACL is operator-controlled here.
- **`mcp_servers:`** — declared MCP servers (HTTP / Streamable HTTP / stdio). Tools become callable from any agent's `tools:`.
- **`storage:`** — backend selection + connection settings. SQLite by default; Postgres opt-in.
- **`concurrency:`** — `max_concurrent_runs`, `max_queue_depth`, optional `max_concurrent_runs_per_user`.
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
2. Read `loomcycle.yaml`: it is short, and each commented block says what uncommenting it gives you.
3. To let agents use files, uncomment `volumes:` and name a directory.
4. Run `loomcycle` to start the server on `127.0.0.1:8787`.
5. Open `http://127.0.0.1:8787/ui` for the Web UI and paste `$LOOMCYCLE_AUTH_TOKEN` on the sign-in page.
