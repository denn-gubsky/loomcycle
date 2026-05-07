# Roadmap

This is the public roadmap. For decision history, regret notes, and per-version commit-by-commit details, see `doc-internal/PLAN.md` (gitignored).

## v0.4.0 — current

**Status: shipped.** Tag `v0.4.0` on `main`. The runtime is now usable as a single-tenant production library.

**What's in v0.4.0:**

- Three providers — Anthropic Messages (with native `cache_control`), OpenAI Chat Completions, Ollama `/api/chat` (tool-tuned models only).
- Nine built-in tools — `Read`, `Write`, `Edit`, `HTTP`, `WebFetch`, `WebSearch`, `Bash`, `Agent` (sub-agent spawning), `Skill` (Approach A static bundling).
- MCP integration — pooled stdio children with auto-respawn, HTTP/SSE clients, per-server allowlists.
- LocalAPI gateway — OpenAPI spec → one tool per operation, scoped to a configured `base_url`.
- Sub-agents via the `Agent` built-in — depth-capped (16), parent host policy + identity inherited via ctx.
- Agent tracking + cancel API — `agent_id` per run, cascade-cancel via `parent_agent_id`, list runs per user.
- Per-agent `max_tokens` config (output budget; covers the case where bundled skills + tool narration eat into a fixed cap).
- Anthropic driver: model alias plumbed from `message_start` into final `Usage.Model` so callers can price runs against the resolved alias (was a silent regression for callers using cache cost dashboards).
- Sub-agent caller-host policy inheritance — children inherit the parent's per-call `allowed_hosts` instead of falling back to the operator's static list (closed a class of "child can't reach localhost" bugs).
- SQLite store — sessions, runs, events; partial indexes for v0.4 sub-agent columns; WAL mode.
- Concurrency — global semaphore + bounded FIFO queue; backpressure → HTTP 429.
- TypeScript adapter (`@loomcycle/client`) shipped on npm.

For usage: see [README](../README.md). For the architecture: see [ARCHITECTURE.md](ARCHITECTURE.md). For tool policy: see [TOOLS.md](TOOLS.md).

## v1.0 — planned

The v1.0 ambition is a **high-load agentic runtime**: 10,000 concurrent agents and sub-agents on a single replica, MCP-orchestrable from external tools (Claude Code, other harnesses), with first-class agent self-improvement through persistent memory and inter-agent messaging.

These items are **outline-only** here. Detailed design (API schemas, storage shapes, retention semantics) lives in feature-branch RFCs at implementation time, not in this roadmap.

### Framework primitives

#### Memory tool

Agent-scoped persistent storage. Backs self-improving agents — an agent can write a fact, a learned heuristic, a counter, or a per-user preference, and read it back on the next invocation. Scope variants (per-agent vs per-session vs per-user) and retention semantics (TTL, size caps, eviction policy) shape the storage backend, which lands on the v1.0 Postgres `Store` adapter.

What's not yet decided: the API shape (key/value vs append-log vs both), TTL semantics, encryption-at-rest, cross-agent read permission model, schema versioning for stored values. RFC at pickup.

#### Channel tool

Persistent inter-agent message bus. One agent writes to a named channel; another reads from it. Powers patterns like "researcher writes findings into `findings/` channel; analyst drains the channel and produces summaries" without making the orchestrator handle handoff. Backend likely Postgres LISTEN/NOTIFY or Redis Streams; durability guarantees TBD.

What's not yet decided: queue vs topic semantics, durability vs at-most-once, ACL (which agent can publish to which channel), backpressure when readers fall behind, dead-letter handling. RFC at pickup.

#### LoomHelp tool

Runtime introspection. Agents query "what tools do I have, what message formats, what skills are loaded, what's the agent_id of my parent" via a single read-only tool. Useful for the Comfort Agents pattern — agents that build their own task plans benefit from being able to inspect their environment before deciding what to do. Output schema is operator-config-derived, with secrets explicitly excluded.

What's not yet decided: output format (JSON schema vs markdown vs both), what counts as a "secret" beyond env-var name patterns, whether the tool can be selectively narrowed at the agent layer.

#### LoomCycle MCP

Loomcycle exposes itself as an **MCP server**. External orchestrators (Claude Code, agentic harnesses, other loomcycle instances) connect to it as an MCP client and:

- Configure agents and spawn runs (alternate front-end to `/v1/runs`).
- Send messages on Channels.
- Read/write Memory entries.
- Call LoomHelp.
- Subscribe to run-event streams (alternate to SSE).

This is the "MCP-configurable" axis: instead of writing YAML and POSTing JSON, an external tool drives loomcycle through standard MCP. Surface area maps roughly 1:1 to the existing `/v1/*` endpoints, with auth via the operator's bearer token translated into MCP's auth scheme.

What's not yet decided: stdio vs HTTP transport (probably both), method naming (resources vs tools), whether MCP clients can register new agents at runtime or only spawn from operator-defined ones, handling of long-lived run streams across MCP's request/response shape.

### High-load runtime work

These are cross-cutting capacity items. Not a single feature; collectively they take the runtime from "single-tenant comfortable on a 4–8 GiB VPS" to "10k concurrent agents per replica."

- **Per-tenant fairness** in the concurrency layer. Currently every caller competes for one global semaphore — a noisy tenant monopolises the pool. Token bucket per `user_id` (or per `tenant_id` once that lands), with a small unfair share for global priorities.
- **Postgres `Store` adapter** replaces single-writer SQLite for multi-replica HA. Schema is already shaped to absorb this (the `Store` interface is provider-agnostic; current SQLite implementation is one of N possible backends).
- **In-memory run-status cache.** Today every `GET /v1/agents/{agent_id}` hits SQLite. At 10k concurrent runs this is a hot path. LRU keyed on `agent_id` with sub-second TTL.
- **Session-lock map GC.** The HTTP server's `sync.Map` of session locks never garbage-collects entries (~32 B per session). Periodic sweep + bounded total entries.
- **OpenTelemetry traces + Prometheus metrics endpoint.** Currently logs only. Per-run trace, per-tool-call span, request rate / queue depth / semaphore-occupancy / provider-RTT histograms.
- **Heartbeat sweeper.** `last_heartbeat_at` is updated by the loop on every iteration but nothing reads it. A sweeper detects crashed runs (no heartbeat for > N minutes) and marks them failed so they don't stay `running` forever.
- **Multi-replica HA.** Postgres for transcripts; Redis for in-flight cancel registry replication. Out-of-process cancel works across replicas via Redis pub-sub.

### Operator posture: sandbox vs agentic, no profile flag

v1.0 keeps a single binary and a single config schema. There is **no** `--profile=sandbox` flag and no separate `loomcycle-server` / `loomcycle-agentic` binaries. Operators compose their own posture by setting individual env vars and YAML keys; the docs ship a cookbook of common shapes.

**Sandbox recipe** — server-style deployment, agents process untrusted prompts, no host filesystem reach:

```bash
LOOMCYCLE_BASH_ENABLED=0
LOOMCYCLE_HTTP_HOST_ALLOWLIST=             # empty — agents reach no hosts unless caller supplies allowed_hosts
LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1      # caller's per-request allowed_hosts is the policy
LOOMCYCLE_READ_ROOT=                       # unset → Read tool refuses every call
LOOMCYCLE_WRITE_ROOT=                      # unset → Write/Edit refuse every call
LOOMCYCLE_SKILLS_ROOT=/srv/loomcycle/skills
```

YAML: every agent's `allowed_tools` lists only the network-restricted set (`HTTP` only with caller-supplied hosts, `WebSearch` with `web_search_filter: drop`, `Agent` for orchestration). Run inside a container; rely on the container for true filesystem isolation.

**Agentic recipe** — the runtime has read/write access to a real working directory, can run shell commands, reaches a broad set of hosts:

```bash
LOOMCYCLE_BASH_ENABLED=1                   # bash on (still NOT a real sandbox; container required)
LOOMCYCLE_BASH_CWD=/work
LOOMCYCLE_HTTP_HOST_ALLOWLIST=api.anthropic.com,api.brave.com,github.com,...
LOOMCYCLE_READ_ROOT=/work
LOOMCYCLE_WRITE_ROOT=/work
LOOMCYCLE_SKILLS_ROOT=/work/.claude/skills
LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST=localhost,127.0.0.1
```

YAML: agents can list any tool. The bearer token (`LOOMCYCLE_AUTH_TOKEN`) is the trust boundary; treat anyone with the token as fully trusted to drive the runtime.

The cookbook in v1.0 will expand this into a full set: development sandbox, single-user agentic dev, multi-tenant SaaS, batch processing, etc.

### Web monitoring frontend

A small frontend on top of the SQLite/Postgres event stream — see runs, drill into transcripts, view token + cost rollups. Not a chat UI; this is operator-facing monitoring. Distinct from the LoomCycle MCP, which is for external orchestration.

### Python adapter

`pip install loomcycle`. Thin client over the HTTP+SSE API, equivalent to the TS adapter at `adapters/ts/`. Deferred until a Python-first downstream consumer materializes.

## Decision principles

These hold across v1.0 work; deviation requires a written justification:

- **Config-driven posture, no profile flag.** Operators compose their security stance from individual env + YAML keys. We do not ship a "sandbox mode" abstraction.
- **One binary stays one binary.** No `loomcycle-server` vs `loomcycle-agent`. Every feature lands in `cmd/loomcycle`. Build artefacts stay singular.
- **MCP-orchestrable.** Whatever surface we expose for agents (Memory, Channel, LoomHelp), we also expose to external MCP clients. Agents and orchestrators play on the same plane.
- **Storage is pluggable.** SQLite for dev/single-tenant; Postgres for multi-replica. Anything new (Memory, Channel) goes through the `Store` interface, not direct SQL.
- **No vendor SDKs in the loop.** Every provider driver is pure HTTP. No bundled binaries; no subprocess auth inheritance.
- **Default-deny stays default-deny.** New tools start invisible to existing agents until they opt in.

## How to contribute to v1.0

Pick an item, write an RFC (one markdown file under `doc-internal/rfcs/<feature>.md`), open a feature branch (`feature-<name>`), follow the chain documented in `CLAUDE.md` (architect → plan → code → tests → review → merge). The RFC is the design step — implementation follows once the RFC is reviewed.

For non-trivial items (Memory tool, Channel tool, Postgres adapter), the RFC should cover:

1. The user-visible surface (API shape, semantics, error cases).
2. The storage / wire shape (schema, message formats).
3. Trust model — who can call this, what's the threat case.
4. Migration plan — what existing code path changes, what stays compatible.
5. Verification — how an operator confirms the feature works end-to-end.

Small features (a new built-in tool, a new provider driver, a fix) skip the RFC and go straight to a feature branch.
