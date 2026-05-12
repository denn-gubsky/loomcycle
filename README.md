<p align="center">
  <img src="docs/assets/logo.png" alt="loomcycle" width="640" />
</p>

<p align="center">
  <strong>Where agents live, talk, and learn.</strong><br/>
  <em>The runtime substrate for agentic systems.</em>
</p>

<p align="center">
  <a href="https://github.com/denn-gubsky/loomcycle/releases"><img alt="release" src="https://img.shields.io/github/v/tag/denn-gubsky/loomcycle?label=release"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <img alt="go" src="https://img.shields.io/badge/go-1.22%2B-00ADD8">
</p>

---

> **🚧 Closed for external contributions until v1.x.** Loomcycle is in active v0.8 → v0.9 → v1.0 development. PRs will be acknowledged but closed without merge during this window. Bug reports for clear-cut defects and security disclosures are still welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the policy.

---

## What it is

LoomCycle is the runtime substrate for production agents — one Go binary that hosts the LLM tool-use loop, governs six providers behind one interface, is configurable as a true managed sandbox or a full agentic dev environment, and is shaped like the kernel of an agentic OS.

A single ~30 MB binary owns the entire `model → tool_use → tool_result → model` cycle, free of vendor SDKs. **Six HTTP-only provider drivers** — Anthropic, OpenAI, DeepSeek, Gemini, Ollama cloud, Ollama local — behind one `Provider` interface. **Twelve built-in tools** — Read, Write, Edit, HTTP, WebFetch, WebSearch, Bash, Agent, Skill, Memory, Channel, Context (the v0.8.7 introspection primitive, with the v0.8.8 `help` op for narrative cross-cutting guidance). **AgentDef + Evaluation** (v0.8.5) — agents can fork themselves and rate the results. **System channels + deferred publish** (v0.8.6) — operator-declared `_system/*` namespace with heartbeats, alarms, and runtime-state signals; any channel's publish can be deferred via `deliver_at`. MCP-native (consuming today; self-exposing in the planned LoomCycle MCP capstone). UNIX-style operator/caller trust separation: the operator config picks the posture, agents can't escape it. Embedded React monitoring UI at `/ui`. Runs on a cheap VPS; scales to multi-replica HA via Postgres + Redis.

Built today for production agentic workloads — built tomorrow for self-evolving multi-agent ecosystems where agents author other agents, talk through channels, and learn from evaluation feedback.

## Two postures, one binary

Same Go binary, same config schema. Operator flips a few env vars to pick the posture.

| Posture | Configuration shape | Use case |
|---|---|---|
| **True managed sandbox** | `LOOMCYCLE_BASH_ENABLED=0`, `LOOMCYCLE_READ_ROOT` / `LOOMCYCLE_WRITE_ROOT` unset, `LOOMCYCLE_HTTP_HOST_ALLOWLIST` empty, `LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1`. Every tool default-deny; agents can only reach what the caller's per-request `allowed_hosts` says. | Shared-server deployments processing untrusted prompts. The runtime survives contact with adversarial input. |
| **Agentic dev environment** | Bash enabled, filesystem roots set to your workspace, broad `allowed_hosts`, optional local Ollama for offline work. | Local development. Internal trusted operators. Single-user research workstation. |

The trust boundary is **operator/caller** — the operator config is the floor, callers can narrow per-request but never widen. The bearer token (`LOOMCYCLE_AUTH_TOKEN`) is the authority. Treat anyone with the token as fully trusted to drive the runtime. For true isolation in the sandbox posture, run loomcycle inside a container or VM — `Bash` is restricted (cwd, env scrub, output bounds, timeouts) but is **not** a kernel-level sandbox.

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
```

## Current and planned

**Shipped through v0.8.8:**

- **v0.8.8** — **`Context.help`** op + bundled topic registry. Tenth op on the Context tool returns a topic index (`{"op":"help"}`) or full markdown body (`{"op":"help","topic":"scopes"}`). Five bundled topics ship embedded in the binary (`loomcycle`, `scopes`, `subagents`, `experimentation`, `system-channels`); operators override or extend via `LOOMCYCLE_HELP_ROOT`. Symlinks under the help root are refused; per-file parse errors soft-skip rather than fail boot.
- **v0.8.7** — **Context** tool: nine ops covering `self` / `tools` / `doc` / `permissions` / `agents` / `lineage` / `evaluations` / `channels` / `history`. Auto-attached to every agent's `allowed_tools` at config-load (opt out via `disable_context: true`).
- **v0.8.6** — **System channels + deferred publish**. Operator-declared `_system/*` channels (heartbeat-1m/5m/1h, alarms/critical/warning/info, runtime-state, provider-events); `SystemPublisher` for loomcycle-authoritative publishes; bearer-authed admin endpoint `POST /v1/_channels/_system/{name}`; general `deliver_at` on any channel's publish with `(visible_at, msg_id)` tuple cursor.
- **v0.8.5** — **Self-Evolution Substrate**: `AgentDef` tool (6 ops — `create` / `fork` / `get` / `list` / `promote` / `retire`) + versioned `agent_defs` with lineage + `Evaluation` tool (5 ops — `submit` / `get` / `list_for_run` / `list_for_def` / `aggregate`). Sub-agent `def_id` pinning for A/B-testing forks. Selection stays policy — loomcycle does NOT auto-promote.
- **v0.8.4** — **Channel** tool (persistent inter-agent message bus — one agent writes to a named channel, another reads, no orchestrator handoff).
- **v0.8.3** — Ollama provider split into `ollama` (cloud) + `ollama-local`. Operators target each backend distinctly in per-agent yaml.
- **v0.8.2 and earlier** — six provider drivers, ten built-in tools (incl. persistent Memory), MCP integration (stdio pool + Streamable HTTP, with lazy retry on first agent call), embedded React Web UI, per-tier provider policy with runtime fallback, agent directory discovery (MD + yaml override), gRPC + HTTP+SSE wire surfaces, TypeScript + Python adapters, SQLite + Postgres backends.

**Planned for v0.8.9 → v1.0:**

- **v0.8.9** — **LoomCycle MCP** (the v0.8.x capstone): loomcycle exposes itself as an MCP server so external orchestrators (Claude Code, agentic harnesses) drive it through standard MCP — Memory / Channel / AgentDef / Evaluation / Context / run-streams surfaced as MCP tools.
- **v0.8.10** — **Question** tool: human-in-the-loop primitive. Three delivery surfaces (built-in Web UI, consumer-side MCP, LoomCycle MCP exposure). Signals flow through `_system/questions/*` channels.
- **v0.8.11** — **Pause / Resume / Snapshot** (the v0.8.x → v0.9.x bridge): runtime-wide quiesce + cross-version-portable JSON snapshot. Precondition for v0.9.x multi-replica HA.
- **v0.9.x** — high-load capacity sweep: per-tenant fairness, OTEL traces, multi-replica HA via Redis cancel pubsub.
- **v1.0** — distribution channels (Homebrew, Docker, Helm), settings UI, operator cookbook of postures.

Full per-version release notes: [`REVISIONS.md`](REVISIONS.md).
Public roadmap with v0.8.x → v1.0 design details: [`docs/PLAN.md`](docs/PLAN.md).

## Architecture (one diagram)

<p align="center">
  <img src="docs/assets/architecture.png" alt="loomcycle architecture — app servers / SDKs at the top, the single Go binary in the middle (wire surfaces → middleware → agent loop → tool dispatcher → store), six LLM providers and external MCP servers at the bottom" width="780" />
</p>

Diagram source: [`docs/architecture.d2`](docs/architecture.d2) (regenerate with `d2 docs/architecture.d2 docs/assets/architecture.png`).

Full request flow, abstractions, and concurrency model: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Adapters

- **TypeScript** — `npm install @loomcycle/client` → see [`adapters/ts/`](adapters/ts/). HTTP+SSE.
- **Python** — `pip install loomcycle` → see [`adapters/python/`](adapters/python/). Async over `grpc.aio`.

## Security highlights

- **No vendor binary** in the loop. Pure HTTP to provider APIs. No subprocess auth inheritance.
- **Default-deny everything.** Every built-in tool is disabled until env-configured. Every agent gets zero tools until `allowed_tools` is set.
- **Two-layer policy + per-request narrowing.** Operator floor in env; agent narrowing in yaml; caller narrowing per-run. Caller can never widen.
- **SSRF defence.** Hostname allowlist + RFC1918/loopback/link-local IP block at the dial layer. Defeats DNS rebinding.
- **Constant-time bearer auth.** `sha256+CTC` on both HTTP and gRPC.
- **`Bash` is restricted, not isolated.** Run inside a container or VM if you need real isolation.

Full security model + the two-layer default-deny walkthrough: [`docs/TOOLS.md`](docs/TOOLS.md).

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — request flow, provider abstraction, agent loop, sub-agents, skills, storage, concurrency, cancellation.
- [`docs/TOOLS.md`](docs/TOOLS.md) — two-layer default-deny model, every built-in tool, MCP / LocalAPI integrations, per-request narrowing.
- [`docs/POSTGRES.md`](docs/POSTGRES.md) — Postgres backend operator guide: configuration, migrations, sqlite→postgres runbook, concurrency benchmark.
- [`docs/GRPC.md`](docs/GRPC.md) — gRPC surface: enablement, wire-shape parity with HTTP+SSE, error mapping, Python adapter quick-start.
- [`docs/PLAN.md`](docs/PLAN.md) — public roadmap: shipped v0.4 → v0.8.8; planned v0.8.9 → v1.0.
- [`REVISIONS.md`](REVISIONS.md) — per-version release notes (v0.4.0 onward).
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — contribution policy (closed for external PRs until v1.x).
- [`CLAUDE.md`](CLAUDE.md) — project guide for agents working in this repo (Claude Code).

## License

Apache-2.0. See [LICENSE](LICENSE).
