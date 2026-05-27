<p align="center">
  <a href="https://loomcycle.dev"><img src="docs/assets/logo.png" alt="loomcycle" width="640" /></a>
</p>

<p align="center">
  <strong>The agentic runtime, in a sidecar.</strong><br/>
  <em>One Go binary alongside your application — hardened agent loop, MCP on both sides, multi-replica HA. Apache-2.0.</em>
</p>

<p align="center">
  🌐 <a href="https://loomcycle.dev"><strong>loomcycle.dev</strong></a> &nbsp;·&nbsp;
  📝 <a href="https://loomcycle.dev/blog/">Engineering blog</a> &nbsp;·&nbsp;
  📐 <a href="https://github.com/denn-gubsky/loomcycle/blob/main/docs/ARCHITECTURE.md">Architecture</a>
</p>

<p align="center">
  <a href="https://github.com/denn-gubsky/loomcycle/releases"><img alt="release" src="https://img.shields.io/github/v/tag/denn-gubsky/loomcycle?label=release"></a>
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>
  <img alt="go" src="https://img.shields.io/badge/go-1.22%2B-00ADD8">
  <a href="https://github.com/sponsors/denn-gubsky"><img alt="sponsor" src="https://img.shields.io/badge/sponsor-%E2%99%A5-ec4899"></a>
</p>

---

> 🌳 **v1.0 shipped — multi-replica HA in production, external contributions open.** The core primitives stabilised through v0.8 → v0.12 → v1.0. We welcome bug reports, security disclosures, feature contributions, downstream consumers, and forks. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

---

## What it is

**The agentic runtime, in a sidecar.** loomcycle is one ~30 MB Go binary that runs *alongside* your application — not inside it. Your app calls loomcycle over HTTP, gRPC, MCP, or via the TypeScript adapter; the agent loop, multi-provider routing, memory and channel primitives, MCP server identity, OpenTelemetry traces, and multi-replica coordination all live in the binary. It's the substrate your agents live on — and your application stays in whatever language you wrote it in.

**The shape that's different.** The agentic-systems market today gives you three choices — embed a Python or TypeScript library inside your process, rent a managed cloud service tied to one vendor's IAM, or proxy your model calls through a gateway that doesn't actually run agents. loomcycle is the fourth shape: a lightweight self-hostable runtime that owns the loop *and* speaks every wire format your stack already uses.

## What's shipped

| Capability | Released in |
|---|---|
| **Six providers, native HTTP, no vendor SDK** — Anthropic, OpenAI, DeepSeek, Gemini, Ollama cloud, Ollama local — behind one `Provider` interface with resolver-based routing per tier and effort | v0.4 → v0.8.x |
| **Nineteen built-in tools** including Claude Code parity (Read, Write, Edit, Grep, Glob, NotebookEdit), HTTP, WebFetch, WebSearch, Bash, Agent, Skill, Memory, Channel, AgentDef, SkillDef, Evaluation, Interruption, Context | v0.4 → v0.8.24 |
| **AgentDef + SkillDef + MCPServerDef substrate** — content-addressed by SHA-256, runtime-mutable, push-at-boot from your container image; verify-or-fork across deployments | v0.8.5 / v0.8.22 / v0.9.x |
| **Vector Memory** with semantic search — `embed: true` on writes, `op: search` on reads. sqlite-vec or pgvector; Voyage / OpenAI / Gemini / nomic-embed on the embedding side | v0.9.0 |
| **MCP on both sides** — loomcycle is both an MCP client (mounts external MCP servers as tools) and an MCP server (Claude Code and external orchestrators drive it via 21 meta-tools) | v0.8.15 |
| **OpenTelemetry** across loop + providers + tools + MCP — no transcripts in spans | v0.10.0 |
| **Per-tenant fairness** on the run-admitting semaphore (single-replica), cluster-wide (multi-replica) | v0.10.1 / v0.12.1 |
| **LLM Gateway + OpenAI-compatible shims** — `POST /v1/_llm/chat`, `POST /v1/chat/completions`, `POST /v1/embeddings`. Drop loomcycle in front of any LangChain / LlamaIndex / n8n / RAG pipeline that speaks OpenAI's wire format | v0.11.0 → v0.11.4 |
| **n8n community package** — `@loomcycle/n8n-nodes-loomcycle`. Five cluster sub-nodes, action nodes, two trigger nodes, six example workflows | v1.x (community) |
| **Anthropic MAX subscription OAuth for dev workflow** — reverse-engineered, dev-only, opt-in; see [`docs/PROVIDERS.md`](docs/PROVIDERS.md) | v0.11.10 |
| **Pause / Resume / Snapshot** — runtime-wide quiesce + cross-version-portable JSON snapshot. In-place upgrades, snapshot-based replica handoff | v0.8.17 → v0.8.18 |
| **Multi-replica HA** — Redis cancel pubsub, cross-replica run status, cluster-wide pause/resume + bus fanout, singleton sweepers, DB-backed session locks. Single binary scales from a cheap VPS to a multi-replica fleet | v0.12.0 → v0.12.5 |
| **UNIX-style trust model** — operator config is the floor; callers narrow per-request but never widen. Bearer auth at the HTTP frontier; sandbox (Posture A) vs operator-trusted (Posture B) selected via env | v0.4 → ongoing |
| **Embedded React Web UI** at `/ui` — Library admin (agents / skills / MCP servers), Activity Monitor, Channels view, audit log | v0.8.21 → v0.11.6 |
| **v1.0 capstone** — docs + hardening pass | v1.0 |

Full per-version log: [`REVISIONS.md`](REVISIONS.md).

## Two postures, one binary

Same Go binary, same config schema. Operator flips a few env vars to pick the posture.

| Posture | Configuration shape | Use case |
|---|---|---|
| **True managed sandbox** | `LOOMCYCLE_BASH_ENABLED=0`, `LOOMCYCLE_READ_ROOT` / `LOOMCYCLE_WRITE_ROOT` unset, `LOOMCYCLE_HTTP_HOST_ALLOWLIST` empty, `LOOMCYCLE_HTTP_CALLER_AUTHORITATIVE=1`. Every tool default-deny; agents can only reach what the caller's per-request `allowed_hosts` says. | Shared-server deployments processing untrusted prompts. The runtime survives contact with adversarial input. |
| **Agentic dev environment** | Bash enabled, filesystem roots set to your workspace, broad `allowed_hosts`, optional local Ollama for offline work. | Local development. Internal trusted operators. Single-user research workstation. |

The trust boundary is **operator/caller** — the operator config is the floor, callers can narrow per-request but never widen. The bearer token (`LOOMCYCLE_AUTH_TOKEN`) is the authority. Treat anyone with the token as fully trusted to drive the runtime. For true isolation in the sandbox posture, run loomcycle inside a container or VM — `Bash` is restricted (cwd, env scrub, output bounds, timeouts) but is **not** a kernel-level sandbox.

## Install

Pick the path that fits. All four ship the same single static binary
plus the v0.11.1 `init` / `doctor` first-run flow. `Context.help
installation` covers each in detail.

```sh
# Homebrew (macOS + Linux)
brew install denn-gubsky/loomcycle/loomcycle

# Docker (v0.11.2+; pull works on amd64 + arm64 including Apple Silicon)
docker pull denngubsky/loomcycle:latest

# go install from source (skips Web UI embedding — for dev only)
go install github.com/denn-gubsky/loomcycle/cmd/loomcycle@latest

# Direct tarball (one of darwin-arm64 / darwin-amd64 / linux-arm64 / linux-amd64)
curl -L https://github.com/denn-gubsky/loomcycle/releases/latest/download/loomcycle-darwin-arm64.tar.gz | tar xz
```

## Quick start

```sh
loomcycle init       # bootstrap ~/.config/loomcycle/loomcycle.yaml + README.md
# set $LOOMCYCLE_AUTH_TOKEN + at least one provider key in your shell rc
loomcycle doctor     # verify env + provider keys + storage + listen
loomcycle            # start the server on 127.0.0.1:8787

# Smoke
curl http://127.0.0.1:8787/healthz
# {"ok":true}

# Open the Web UI (one-time per browser session — sets HttpOnly cookie)
open "http://127.0.0.1:8787/ui?token=$LOOMCYCLE_AUTH_TOKEN"
```

Real call (from another terminal):

```sh
curl -N http://127.0.0.1:8787/v1/runs \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"Hello"}]}]}'
```

Build from a checkout (for development):

```sh
make build-all       # UI + binary in one shot; output → ./bin/loomcycle
./bin/loomcycle --config loomcycle.example.yaml
```

**Multi-replica cluster demo (v0.12.x):** for a one-command `docker compose up` cluster (2 loomcycle replicas + Postgres + nginx LB) with a verify script, see [`examples/cluster/README.md`](examples/cluster/README.md). Full operator runbook in [`docs/MULTI-REPLICA.md`](docs/MULTI-REPLICA.md).

## Current and planned

**Shipped through v0.11.x:**

- **v0.11.12 — Hash CLI yaml-name mode + Channels UI source-tag filters.** Two small DX items bundled. (a) `loomcycle hash agent --config <yaml> <name>` extends the existing v0.9.x path-mode hash CLI with a name-lookup mode — operators whose agents live in `loomcycle.yaml` (not standalone `.md` files) can now compute the local `content_sha256` to compare against `AgentDef.verify` for pre-deploy drift detection. Path-mode unchanged. (b) Web UI Channels view filter chip row gains `yaml` / `runtime` / `orphan` source-tag chips alongside the existing scope chips (`all` / `_system/*` / `global` / `user` / `agent`) — closes the v0.11.5 UX gap where operators had to scan visually for source on each row. Zero server-side changes; web UI 0.7.6 → 0.7.7; `@loomcycle/client` stays at 0.11.5.
- **v0.11.11 — Explicit no-guarantee disclaimer for OAuth-dev.** Documentation + visible-warning patch on top of v0.11.10. Four operator-facing surfaces now carry consistent explicit language about the reverse-engineered OAuth flow: `docs/PROVIDERS.md` ⚠ NO GUARANTEES callout, CLI `loomcycle anthropic login` prints the full disclaimer block before any auth work, boot-log warning, README v0.11.10 entry. Zero behavior change.
- **v0.11.10 — Anthropic OAuth-dev stealth-mode parity.** ⚠ **NO GUARANTEES.** The Anthropic OAuth flow used by this provider is not an official integration — it's reverse-engineered (Pi reference + loomcycle replication). loomcycle mimics Claude Code's wire shape so calls pass Anthropic's subscription-billing detection, but Anthropic can change the flow, the wire shape, or the detection at any time and the integration will break. **Operators are running this against their own Claude Pro/Max subscription; Anthropic's terms historically restrict programmatic use outside their SDK; operators carry all risk including account flag/revocation. loomcycle and its maintainers offer no warranty, no liability, and no support guarantees for this path.** Resolution of any account issues is between the operator and Anthropic. If those terms are unacceptable, use the production `anthropic` provider (API key) instead. See `docs/PROVIDERS.md` for the full risk acknowledgement. — Closes the v0.11.9 deferrals by converging the live-data findings against a real MAX subscription. Three wire-shape adaptations were needed for Anthropic's OAuth-billing path to accept loomcycle's calls: (1) `redirect_uri` must use literal `localhost` not `127.0.0.1` (string match against the registered client_id whitelist); (2) token-exchange body requires a `state` field (non-standard for OAuth2 but Anthropic enforces it); (3) the system prompt must lead with the verbatim `"You are Claude Code, Anthropic's official CLI for Claude."` block — without it the validator returns a misleading `"messages: Input should be a valid array"` 400. Plus two mechanical fixes: `CanonicalizeToolName` now wired into `MaskOutbound` (lowercase `allowed_tools: [read, write]` in yaml canonicalize to `Read`, `Write` outbound); `ErrSubscriptionQuotaExhausted` promoted from stub to real detection on the synchronous error path (429+"subscription" body wraps with a type that preserves the original error text — so `internal/providers/errclass.go`'s regex still matches and the existing tier-fallback path still fires — and adds `Is()` so `errors.Is(err, ErrSubscriptionQuotaExhausted)` matches for callers). Cache-control verification: writes report under OAuth but reads aren't served — kept on for Pi wire-parity, documented as 25% per-call premium. Live verified end-to-end: `provider: anthropic-oauth-dev` agent gets `pong` from claude-sonnet-4-6 against the operator's MAX subscription. v0.11.9 was never tagged independently; v0.11.10 supersedes it functionally.
- **v0.11.9 — Anthropic OAuth-dev provider (research/dev only).** Opt-in subscription-billed Anthropic provider via reverse-engineered OAuth (Pi's `pi-ai` package is the reference; github.com/earendil-works/pi, 51K stars). Strategic shift: research workloads at ~$750-$3,750 per 100-iteration cycle move to $200/month MAX subscription billing without changing the production posture for paying customers. RFC-locked at `doc-internal/rfcs/anthropic-oauth-dev.md`. **Opt-in via `LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1` + `loomcycle anthropic login`.** New provider ID `anthropic-oauth-dev` (separate from production `anthropic`). HTTP transport wraps the existing Anthropic driver — strips `x-api-key`, sets `Authorization: Bearer <token>` (from a background refresher with 5-min slack), appends `claude-code-20250219,oauth-2025-04-20` to `anthropic-beta`, sets `user-agent: claude-cli/2.1.75` (overrideable via `LOOMCYCLE_CLAUDE_CODE_VERSION`). **All loomcycle tools admissible** — the 10-tool Claude-Code canonical overlap passes through unchanged; loomcycle-only built-ins (Memory / Channel / Agent / AgentDef / etc.) are exposed under the `mcp__loomcycle__*` wire mask so Anthropic's subscription-billing layer treats them as MCP tools; real `mcp__*` MCP tools pass through unchanged. New CLI: `loomcycle anthropic {login, status, logout}`, all gated on the env var. Token persistence at `~/.config/loomcycle/anthropic-oauth.json` (chmod 0600 enforced). New `docs/PROVIDERS.md` (300+ lines) covers operator setup + risk acknowledgement (TOS gray zone, account-revocation risk, drift exposure, single-machine deployment). **NOT for production deployment.** Multi-replica HA explicitly unsupported. v0.11.10 follow-up will land stealth-mode parity (system-prompt adaptation, cache-control rules, MCP schema audit) once OAuth shell is operator-validated against a real MAX subscription.
- **v0.11.8 — Multi-agent fan-out: `Agent.parallel_spawn` op + per-agent `max_concurrent_children` cap.** Formalizes the locked v0.9.x backlog item from `langchain-comparison.md` Tier A. JobEmber's job-searcher has been spawning sub-agents sequentially in prod for months; v0.11.8 gives the model a first-class API to fan out concurrently. New `op` discriminator on the existing `Agent` tool (matches the Memory/Channel/AgentDef multi-op convention) — `op:"spawn"` (default, omittable) keeps the v0.4.0 single-child shape byte-identical; `op:"parallel_spawn"` takes `{spawns: [{name,prompt,def_id?}, ...]}` and returns a JSON envelope `{results:[{index,agent,ok,output?,error?}]}` in input order when ALL children complete. Per-child errors stay inside the envelope (parent runs never torn down by child failures). Concurrency cap layered: per-agent `max_concurrent_children: N` yaml/substrate field (default 4) throttles parallel goroutines per call; hard ceiling `MaxParallelSpawns=32` refuses runaway-spawns arrays up-front; depth guard fires once per call. New bundled `Context.help` topic `fan-out-patterns` explains when to use parallel vs sequential vs Channel.publish. Web UI Library modal gains `max_concurrent_children` number input. Zero adapter changes — TS stays at 0.11.5. 12 new unit tests + end-to-end smoke verified.
- **v0.11.7 — Post-v0.11.6 polish.** Three small unrelated improvements bundled into one release. (a) Promoted `--fg-muted` + `--bg-input` from inline-fallback-only into first-class `:root` CSS tokens — continues the v0.11.6 theming-consistency treatment. (b) Library modal: the v0.11.6 custom-tier warning was buried in the per-tier models grid at the bottom; operators forking just to tweak `system_prompt` could miss it. Hoisted to the top of AgentFields so it's visible the moment the modal opens. (c) `publish-ts-adapter` workflow now skips cleanly on Web-UI-only or binary-only releases (when `adapters/ts/package.json` version doesn't match the tag) instead of hard-failing the publish job. Zero server-side changes; `@loomcycle/client` stays at 0.11.5; web UI 0.7.4 → 0.7.5.
- **v0.11.6 — Library admin modal: fully structured form for agent + skill definitions.** The v0.10.4 hybrid (structured inputs + JSON catch-all textarea) was failing operators in real use: raw newlines inside `system_prompt` produced invalid JSON, a single missing comma sank the whole submit, and the JSON catch-all hid the schema behind manual typing. Every editable agent overlay field is now its own structured input — `system_prompt` is a markdown textarea (raw newlines preserved), `allowed_tools`/`skills`/`providers` are comma-separated text inputs, `max_tokens`/`max_iterations`/`memory_quota_bytes` are number inputs, `memory_scopes` is a checkbox group, and the per-tier `models` map is a small dynamic table editor (three fixed tier slots: low/middle/high). The JSON catch-all is gone — the modal is now the authoritative schema view. Skill modal gets a small polish pass (markdown body class renamed `.library-prompt-textarea` for intent clarity; inline hint text added). MCP-server modal unchanged (already fully structured since v0.10.4). Zero server-side changes — wire shape identical to v0.11.5. Pure Web UI release; binary functionally identical to v0.11.5. `@loomcycle/client` stays at 0.11.5.
- **v0.11.5 — yaml-static channels + memory + Web UI CRUD.** The last v0.11.x slice before the multi-replica HA capstone. Two operator pain points: n8n integration tests can now programmatically create channels + pre-seeded memory entries as fixtures (no yaml + restart between tests), and static substrate deployments can declare the entire substrate — agents, channels, memory entries — purely in yaml. **yaml additions:** `channels.<name>.description: "..."` for operator docs, plus a new `memory.entries:` block with `{scope, scope_id, key, value, embed?}` rows pre-seeded on boot (idempotent — existing rows are preserved; `embed: true` triggers a synchronous embed via the configured embedder). **Channel admin HTTP CRUD:** `POST /v1/_channels` / `PATCH /v1/_channels/{name}` / `DELETE /v1/_channels/{name}` against a new `channels` substrate table; yaml-declared channels refuse mutations with HTTP 409 `channel_yaml_immutable`. `GET /v1/_channels` now tags each row with `source: "yaml" | "runtime" | "orphan"`. **Memory entry admin HTTP CRUD:** `PUT /v1/_memory/scopes/{scope}/{scope_id}/keys/{key}` (idempotent upsert, optional `?embed=true`) + `DELETE` (204 even on missing rows). **Web UI:** Channels view gains "+ New channel" + per-row Edit/Delete (runtime only) + source chips; Memory view gains "+ New entry" + per-row Edit/Delete with a JSON-value editor + optional embed toggle + TTL. **TS adapter** (`@loomcycle/client` 0.11.4 → 0.11.5): 5 new methods (`createChannel`/`updateChannel`/`deleteChannel`/`setMemoryEntry`/`deleteMemoryEntry`) + 4 typed exports + 12 new vitest tests. **Storage:** new `channels` table on sqlite + postgres (0021 migration); 4 new `Store` interface methods. Cursor namespace untouched — runtime channels reuse `channel_messages` + `channel_cursors`; delete cascades both. Two new bundled `Context.help` sections (`channel-admin` runtime-substrate CRUD; `vector-memory` yaml-static entries + admin PUT/DELETE). 42 → 47 methods on `@loomcycle/client`.
- **v0.11.4 — OpenAI Embeddings compatibility shim.** Closes the OpenAI-ecosystem story v0.11.3 started. New `POST /v1/embeddings` endpoint serves OpenAI's exact wire shape; every RAG tool / vector DB integration / LangChain `OpenAIEmbeddings` consumer / "use OpenAI embeddings" tutorial works by changing only the base URL + auth token. Dispatches to the single configured `providers.Embedder` (the same instance Memory tool uses internally for `embed:true`) — no resolver path, no tier overlay, no streaming since OpenAI's embeddings API is synchronous. **Wire translation:** `input` polymorphic (string OR string[]); tokenized inputs (number arrays per OpenAI) refused with a clear error since loomcycle's substrate embedders (OpenAI / Gemini / Voyage-via-Anthropic) accept text only. `encoding_format: "float"` (default) emits each vector as a JSON array; `"base64"` packs each float32 little-endian then base64-encodes per OpenAI spec (~25% smaller wire bytes on 1536-dim vectors). `model` echoed in the response for drop-in compatibility — the configured embedder always runs, and the audit log records both requested + served so operators spot drift. `user` field auto-maps to loomcycle's per-user quota tracking. `dimensions` accepted-but-ignored in v1 (lands when the `providers.Embedder` interface grows it). **No embedder configured** → HTTP 503 with a pointer at `memory.embedder.*` yaml. **Same security policy** as the v0.11.3 chat shim: per-user semaphore via `AcquireForUser`, bearer-authed admin scope, structured audit log line per request. 10 new Go tests + 6 new TS tests; full Go suite + 144 TS tests pass. New `embeddings()` method on `@loomcycle/client` with 4 new typed exports (`LLMEmbeddingsOptions` / `LLMEmbeddingsResponse` / `LLMEmbeddingItem` / `LLMEmbeddingsUsage`) — consumers using the typed adapter get richer typing than dropping the OpenAI SDK at loomcycle. Extends the bundled `openai-compat` Context.help topic with an Embeddings section + Python/TypeScript SDK examples. Zero changes to `internal/providers/`, `internal/resolve/`, `internal/loop/`, or the embedder interface itself. `@loomcycle/client` 0.11.3 → 0.11.4 (41 → 42 methods).
- **v0.11.3 — OpenAI Chat Completions compatibility shim.** New `POST /v1/chat/completions` endpoint serves OpenAI's exact wire shape; consumers using the OpenAI SDK (Python / TypeScript / Aider / Goose / Continue / Cursor / Cody / every "use OpenAI as your LLM" tutorial) point at loomcycle by changing only the base URL + auth token. Same dispatch path as the native `/v1/_llm/chat` (resolver routing, per-user quota, audit logging all in one place via the new `prepareGatewayDispatch` helper); the shim is a pure wire-format translator. **Wire translation:** OpenAI's `messages[].content` polymorphic field (string / array of text parts / null) → flat string. OpenAI's wrapped `tools[]` envelope → loomcycle's flat ToolSpec. OpenAI's JSON-string `function.arguments` → parsed object. Response: native content blocks → `choices[0].message.content` + `tool_calls` (OpenAI function envelope). `stop_reason` → `finish_reason` per OpenAI spec (`end_turn`→`stop`, `max_tokens`→`length`, `tool_use`→`tool_calls`). **Streaming**: `data: <json>` chunks in the `chat.completion.chunk` shape, terminated by `data: [DONE]` (bare `data:` lines, NO named SSE events — matches OpenAI's protocol). **Routing extensions** via namespaced fields: `loomcycle_provider`, `loomcycle_tier`, `loomcycle_user_id`, `loomcycle_user_tier`. OpenAI's standard `user` field auto-maps to `loomcycle_user_id` so SDK callers get per-user quota tracking for free. **Accepted-but-ignored** OpenAI fields: `n`, `presence_penalty`, `frequency_penalty`, `top_p`, `seed`, `response_format`, `logit_bias`, `tool_choice`, `top_logprobs`, `stop` — accepted so SDKs don't validation-error, ignored because loomcycle doesn't apply them. Refactored `handleLLMChat` to extract `prepareGatewayDispatch` (~70 LOC moved); both handlers share validation/resolve/quota/audit. 11 new tests covering happy path / tool-call / streaming / array-content / ignored-fields / user-id mapping / finish-reason matrix / auth / validation. New bundled `Context.help openai-compat` topic with Python + TypeScript SDK examples. Zero new substrate logic; zero changes to `internal/providers/`, `internal/resolve/`, `internal/loop/`. `@loomcycle/client` 0.11.2 → 0.11.3 (lockstep version bump; no method changes — consumers using `@loomcycle/client` should still prefer `llmChat()` / `llmStream()` over the OpenAI SDK shim for richer typing).
- **v0.11.2 — Docker image + Homebrew formula polish.** Closes the install-path loop opened by v0.11.1. **Multi-arch Docker image** at `docker.io/denngubsky/loomcycle` (linux/amd64 + linux/arm64; ~6 MB total based on `gcr.io/distroless/static:nonroot`; no shell, no package manager, runs as uid 65532). New `Dockerfile` for local builds + `Dockerfile.release` for goreleaser's pre-built-binary path; both produce identical runtime images. New `docker-compose.example.yaml` at the repo root with mount + env-var + port-mapping defaults plus a commented-out Postgres upgrade block. **goreleaser `dockers:` + `docker_manifests:`** publish multi-arch manifests on every release tag — stubbed behind a `DOCKER_PUBLISH_ENABLED` repo variable so the pipeline ships tarballs + brew formula alone until the operator configures Docker Hub credentials (then flipping the var enables Docker push without further changes). **Brew formula caveats refreshed** to reference `loomcycle init` / `loomcycle doctor` (v0.11.1 commands) instead of the obsolete "drop your loomcycle.yaml" manual flow. New bundled `Context.help installation` topic walks all four install paths (Homebrew, Docker, direct tarball, `go install`) with verification + troubleshooting. README "Install" section promoted above "Quick start" so the four paths appear before the build-from-source flavor. Zero Go code changes — pure release-pipeline + docs work. `@loomcycle/client` 0.11.1 → 0.11.2 (lockstep version bump; no method changes).
- **v0.11.1 — First-run UX: `loomcycle init` + `loomcycle doctor` + config auto-discovery.** Closes the "bare binary fails to start" gap that bit every new operator. **`loomcycle init`** writes `~/.config/loomcycle/loomcycle.yaml` + `~/.config/loomcycle/README.md` from bundled assets — auto-on minimal wizard (3 questions: provider / env-var / listen-addr) when stdin is a TTY, non-interactive otherwise. CLAUDE.md security rule §2 holds: the wizard prints env-var lines for the operator to paste into their shell rc; never writes secrets to disk. **`loomcycle doctor`** runs 6 checks (config found + parses, auth token, per-provider API-key env vars, storage backend writable, listen address bindable) and prints `[PASS]` / `[WARN]` / `[FAIL]` per check; exits 0 clean, 1 on any FAIL. **Config auto-discovery** — when `--config` is left at default AND `./loomcycle.yaml` is absent, the binary walks `$XDG_CONFIG_HOME/loomcycle/loomcycle.yaml` → `~/.config/loomcycle/loomcycle.yaml`. When nothing's found, a friendly first-run hint replaces the old confusing "open loomcycle.yaml: no such file" error. Explicit `--config /path` semantics unchanged. **Bundled documentation**: the example yaml moved to `cmd/loomcycle/embedded/` (symlinked back at repo root) and is `//go:embed`'d alongside a new per-machine quickstart `README.md` (distinct from the repo's existing `docs/CONFIGURATION.md` deep-dive — they're complementary). New bundled `Context.help getting-started` topic for the agent-facing surface. Purely additive — zero HTTP surface changes, zero schema changes. 7 new init tests + 10 new doctor tests pass. `@loomcycle/client` 0.11.0 → 0.11.1 (lockstep version bump; no method changes).
- **v0.11.0 — LLM Gateway: direct provider routing without the agent loop.** New `POST /v1/_llm/chat` endpoint exposes the resolver + provider auth + retry layer as a direct LLM call surface. Bypasses the agent loop entirely — consumers who only need provider routing (no tools, no memory, no agent semantics) skip the ~50-200 ms per-turn overhead of a full `runStreaming` spawn. The immediate use case is **n8n's AI Agent Chat Model slot**: `@loomcycle/n8n-nodes-loomcycle` will ship a `LoomCycleChatModel` cluster sub-node consuming the gateway directly, replacing the v0.10.x "passthrough agent" workaround. The broader product positioning competes with LiteLLM / Portkey: one credential + one quota + one observability surface across all providers (Anthropic, OpenAI, DeepSeek, Gemini, Ollama, Ollama-cloud). **Wire shape:** LangChain-friendly request (flat `messages[]` with `tool_calls` / `tool_call_id` correlation) → Anthropic-style content-block response (`{type:"text"|"tool_use"}`). **Routing precedence:** `provider+model` (explicit pin) > `provider` alone > `model` alone > resolver default. **Tool calling works:** the existing per-provider schema translation (Anthropic input_schema vs OpenAI function.parameters vs Gemini function_declarations) is reused from each driver's `buildRequestBody()` — zero new translation code. **Streaming SSE** mirrors Anthropic's event names (`provider_chosen` → `content_block_start`/`delta`/`stop` → `message_delta` → `done`). **Per-user quotas** honored when `user_id` is set; anonymous calls bypass the per-user cap. **Audit:** lightweight structured log line per request + OTEL span when configured (v0.11.1 follow-up will add a dedicated `gateway_events` table). **No new server packages** — handler lives in `internal/api/http/llm_gateway.go` reusing `Provider.Call`, `Resolver.Resolve`, `Semaphore.AcquireForUser`, and the existing SSE writer. **No runs-table row per gateway call** — gateway calls are too high-cardinality (n8n workflows fire dozens per execution). `@loomcycle/client` 0.10.4 → 0.11.0 adds `llmChat()` + `llmStream()` typed methods (36 → 41 methods total). 8 new Go tests + 10 new TS tests. New bundled `Context.help llm-gateway` topic.

**Shipped before v0.11.x** (per-feature detail in [`REVISIONS.md`](REVISIONS.md)):

- **v0.10.x — production-grade-ops sweep**: OpenTelemetry distributed traces (v0.10.0); per-tenant fairness on the run-admitting semaphore + `GET /v1/_concurrency/stats` admin endpoint (v0.10.1); Voyage AI embedder + `sqlite_vec` build tag + heartbeat-sweeper flake fix (v0.10.2); typed Library v2 enumeration on `@loomcycle/client` (v0.10.3); Web UI Library admin — manual CRUD on agents / skills / MCP servers via `/ui/library` (v0.10.4).
- **v0.9.x — vector memory + substrate maturity**: **Vector Memory** with semantic search, pluggable embedders (OpenAI / Gemini / Voyage), pgvector + sqlite-vec backends, admin reembed flow (v0.9.0); transcript first-cycle visibility — `system_prompt` + `user_input` events at run start (v0.9.1); n8n integration Phase 0 + 1 (`GET /v1/_channels`, `GET /v1/users/{id}/agents/stream`); content signatures on AgentDef + SkillDef rows + `verify` op + `loomcycle hash` CLI; dynamic MCP server registration (`MCPServerDef`); Channel CRUD wire-API (publish / subscribe / peek / ack on HTTP + gRPC + MCP + TS); Web UI Library v2 + dynamic-agent resolver consolidation (`internal/lookup`) + transcript USER/SYSTEM card disambiguation (v0.9.3).
- **v0.8.x — substrate + infrastructure foundation**: Ollama provider split into `ollama` cloud + `ollama-local` (v0.8.3); `Channel` tool — persistent inter-agent message bus (v0.8.4); Self-Evolution Substrate — `AgentDef` tool with versioned `agent_defs` + lineage + `Evaluation` tool, sub-agent `def_id` pinning (v0.8.5); system channels + deferred publish — `_system/*` namespace, `SystemPublisher`, `deliver_at` (v0.8.6); `Context` tool with 9 ops (v0.8.7); `Context.help` op + bundled topic registry with FS overlay (v0.8.8); Gemini schema sanitizer — `$ref` inline + `oneOf`/`anyOf`/`allOf` merge + sqlite migration ordering fix (v0.8.9–v0.8.10); process-resource metrics sampler + `/v1/_metrics/*` admin API (v0.8.11); strip `reasoning_content` on cross-provider fallback + `EventReasoningInvalidated` (v0.8.12); pin-after-success fallback policy + per-run MCP bearer tokens (`${run.user_bearer}`) + auto-version from `runtime/debug` (v0.8.13–v0.8.14); **LoomCycle MCP server** — loomcycle as a stdio MCP server with 20 meta-tools + new `connector.Connector` Go interface (v0.8.15); **`Interruption` tool** — human-in-the-loop primitive with `ask` / `notify` / `cancel` ops + Web UI / MCP / CLI delivery backends (v0.8.16); **Pause / Resume / Snapshot** — runtime-wide quiesce + cross-version-portable JSON snapshot envelope + CLI mirrors + Web UI surface (v0.8.17); cross-transport hardening — real Connector method bodies + 9 gRPC RPCs + Python adapter v0.6.0 + TypeScript adapter v0.8.18 (v0.8.18); Activity Monitor + audit view + UI polish — `/ui/activity` charts, `/ui/audit` over `GET /v1/_events`, splitter panes, await-state tints (v0.8.21); **`SkillDef`** runtime-mutable skill substrate (v0.8.22); `SkillDef` + `AgentDef` on every wire surface — HTTP + gRPC + MCP + TS + Python with `SubstrateToolRefusedError` (v0.8.23); **Claude Code parity built-ins** — `Grep` + `Glob` + `NotebookEdit` (v0.8.24).
- **v0.8.2 and earlier — foundation**: six provider drivers (Anthropic, OpenAI, DeepSeek, Gemini, Ollama cloud, Ollama local); ten built-in tools including persistent `Memory`; MCP integration (stdio pool + Streamable HTTP with lazy retry on first agent call); embedded React Web UI at `/ui`; per-tier provider policy with runtime fallback; agent directory discovery (`.md` files + yaml override); gRPC + HTTP+SSE wire surfaces; TypeScript + Python adapters; SQLite + Postgres backends.

**Planned for v0.9.x → v1.0:**

- **v0.9.x** — **Semantic Memory** (vector retrieval) extends the v0.8.0 Memory tool with a `search` op and an `embed: true` field on `set`; new `memory_embeddings` table joined on `(scope, scope_id, key)`; pgvector (Postgres) + sqlite-vec (SQLite) backends gated on operator env opt-in; server-side embedding via a new `providers.Embedder` interface (Anthropic + OpenAI + Gemini drivers). Snapshot Memory section schema reserves the optional `embedding` field today so v0.8.17 snapshots round-trip cleanly into a future loomcycle that has vector ops.
- **v0.9.x** — high-load capacity sweep: per-tenant fairness, OTEL traces, multi-replica HA via Redis cancel pubsub, in-memory run-status cache, heartbeat-sweeper hardening.
- **v1.0** — distribution channels (Homebrew, Docker, Helm), settings UI, operator cookbook of postures.

Full per-version release notes: [`REVISIONS.md`](REVISIONS.md).
Public roadmap with v0.8.x → v1.0 design details: [`docs/PLAN.md`](docs/PLAN.md).

## Architecture

Three diagrams cover different views of the same runtime:

<p align="center">
  <img src="docs/assets/architecture.png" alt="loomcycle architecture — clients at the top (app servers, CLIs, TS/Python SDKs, Claude Code & MCP orchestrators, LangChain/n8n via OpenAI-compat shim), the single Go binary in the middle (1..N replicas; five wire surfaces incl. HTTP+SSE / gRPC / Web UI / MCP server with 33 meta-tools / LLM Gateway → bearer auth + concurrency semaphore + per-user fairness → 36-method connector.Connector → agent loop → tool dispatcher with 19 built-in tools + MCP client transport + sub-agent runner → SQLite/Postgres store covering sessions, runs, events, memory, channels, substrate tables, replicas+user_quotas+runtime_state+hooks), OpenTelemetry sidecar emitting spans, and external services at the bottom (seven LLM providers including anthropic-oauth-dev, three embedders, external MCP servers cloud)" width="780" />
</p>

Diagram source: [`docs/architecture.d2`](docs/architecture.d2) (regenerate with `d2 docs/architecture.d2 docs/assets/architecture.png`).

**Connector detail** — the v0.8.15 `Connector` abstraction layer (the pink block in the middle of the main diagram) is the architectural anchor every wire transport dispatches through. The detail diagram enumerates all 36 methods + shows which transports IMPLEMENT, CONSUME, and MIRROR the interface:

<p align="center">
  <img src="docs/assets/architecture-connector.png" alt="connector.Connector interface with 36 methods grouped by domain (run lifecycle, agent registry, substrate tools, channel CRUD, pause/snapshot, hook registry) — HTTP server IMPLEMENTS as the canonical business logic, MCP and gRPC servers CONSUME via direct Go method dispatch, TypeScript and Python adapters MIRROR over the HTTP wire" width="780" />
</p>

Source: [`docs/architecture-connector.d2`](docs/architecture-connector.d2).

**Multi-replica cluster mode (v0.12.x)** — when `LOOMCYCLE_REPLICA_ID` is set per process and the Postgres backend is used, loomcycle runs as a cluster behind any HTTP load balancer. The shared Postgres doubles as the LISTEN/NOTIFY backplane for cross-replica cancel, pause/resume, run-state fanout, and quota notifications. SQLite refuses cluster mode at boot.

<p align="center">
  <img src="docs/assets/architecture-cluster.png" alt="multi-replica cluster deployment — clients hit an HTTP load balancer (nginx/Caddy/Traefik/HAProxy/ELB, SSE-friendly), which round-robins across N replicas each with a distinct LOOMCYCLE_REPLICA_ID + 30s heartbeat, all sharing a Postgres database that holds both the substrate tables (replicas, user_quotas, runs.replica_id, runtime_state, hooks added in v0.12.x) and the LISTEN/NOTIFY backplane carrying cancel/pause/runstate/channel/quota/hook topics, with singleton sweepers gated via pg_try_advisory_lock" width="780" />
</p>

Source: [`docs/architecture-cluster.d2`](docs/architecture-cluster.d2). Operator runbook: [`docs/MULTI-REPLICA.md`](docs/MULTI-REPLICA.md). Demo: [`examples/cluster/README.md`](examples/cluster/README.md).

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

Repo-side docs (this directory):

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — request flow, provider abstraction, agent loop, sub-agents, skills, storage, concurrency, cancellation.
- [`docs/TOOLS.md`](docs/TOOLS.md) — two-layer default-deny model, every built-in tool, MCP / LocalAPI integrations, per-request narrowing.
- [`docs/MCP_INTEGRATION.md`](docs/MCP_INTEGRATION.md) — end-to-end MCP HTTP pipeline: request lifecycle, `${run.user_bearer}` substitution, model-visibility boundary, recipe for wrapping a REST API as an MCP server consumable by loomcycle.
- [`docs/MCP_SERVER.md`](docs/MCP_SERVER.md) — register loomcycle as an MCP server in Claude Code / Claude Desktop: copy-paste config snippets for Docker / Homebrew / direct-binary transports + the `loomcycle mcp install` helper.
- [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) — operator config guide: provider/tier/user_tier resolution rules, four cookbook patterns (single/multi-provider × single/multi-user-tier), `models:` alias map, and the agent `.md` frontmatter field reference.
- [`docs/POSTGRES.md`](docs/POSTGRES.md) — Postgres backend operator guide: configuration, migrations, sqlite→postgres runbook, concurrency benchmark.
- [`docs/GRPC.md`](docs/GRPC.md) — gRPC surface: enablement, wire-shape parity with HTTP+SSE, error mapping, Python adapter quick-start.
- [`docs/PLAN.md`](docs/PLAN.md) — public roadmap: shipped v0.4 → v0.8.12; planned v0.8.13 → v1.0.
- [`REVISIONS.md`](REVISIONS.md) — per-version release notes (v0.4.0 onward).
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — contribution policy (closed for external PRs until v1.x).
- [`CLAUDE.md`](CLAUDE.md) — project guide for agents working in this repo (Claude Code).

In-binary docs (bundled `Context.help` topics — agents read these directly; operators hit `GET /v1/_help/<topic>` against a running instance):

- `installation` — all four install paths (Homebrew, Docker, `go install`, direct tarball) with verification + troubleshooting.
- `getting-started` — first-run walkthrough (`init` → set env vars → `doctor` → run).
- `llm-gateway` — direct LLM routing endpoint (v0.11.0; for n8n + LangChain consumers).
- `openai-compat` — drop-in OpenAI SDK shims (v0.11.3 chat + v0.11.4 embeddings) with Python + TypeScript examples.
- `fairness` — per-user concurrency quota policy.
- `observability` — OTEL trace export setup.
- `vector-memory`, `voyage-embedder`, `sqlite-vec` — Vector Memory backends.
- `dynamic-mcp` — register MCP servers at runtime.
- `bash-security` — Bash tool's restricted-not-isolated security posture.

Full list via `GET /v1/_help` against a running instance.

## Sponsor

If loomcycle is useful to you or your team, [GitHub Sponsors](https://github.com/sponsors/denn-gubsky) helps fund continued development. Individual supporters and corporate sponsors both welcome.

The runtime stays Apache-2.0 either way. Sponsorships directly fund the v0.12 → v1.0 runway: multi-replica HA, Helm chart, operator cookbook, settings UI, and the per-tier sustained engineering needed to keep the binary small and the substrate stable.

Current sponsors are listed in [`BACKERS.md`](BACKERS.md) (and at [loomcycle.dev/sponsors](https://loomcycle.dev/sponsors) with logos).

## License

Apache-2.0. See [LICENSE](LICENSE).
