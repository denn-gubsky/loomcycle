<p align="center">
  <a href="https://loomcycle.dev"><img src="docs/assets/banner.png" alt="loomcycle" width="640" /></a>
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

> 🌳 **Active development toward v1.0.** The core primitives stabilised through v0.8 → v0.16 — multi-replica HA, the substrate Defs (Agent/Skill/MCPServer/Schedule/Webhook/MemoryBackend), A2A interoperability, inbound webhooks, pluggable memory + a memory layer, and a synthetic code provider. **v0.17.0** shipped **OSS multi-tenant authorization** (per-principal bearer tokens + a role-aware Web UI) — see "Planned" below. **The feature set is complete: no new features are planned before v1.0** — we're finishing the test / QA + hardening pass, then tagging 1.0 (a pure hardening + distribution milestone — Homebrew / Docker / Claude Code plugin). We welcome bug reports, security disclosures, feature contributions, downstream consumers, and forks. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

---

## What it is

**The agentic runtime, in a sidecar.** loomcycle is one sub-40 MB Go binary that runs *alongside* your application — not inside it. Your app calls loomcycle over HTTP, gRPC, MCP, or via the TypeScript adapter; the agent loop, multi-provider routing, memory and channel primitives, MCP server identity, OpenTelemetry traces, and multi-replica coordination all live in the binary. It's the substrate your agents live on — and your application stays in whatever language you wrote it in.

**The shape that's different.** The agentic-systems market today gives you three choices — embed a Python or TypeScript library inside your process, rent a managed cloud service tied to one vendor's IAM, or proxy your model calls through a gateway that doesn't actually run agents. loomcycle is the fourth shape: a lightweight self-hostable runtime that owns the loop *and* speaks every wire format your stack already uses.

## What's shipped

| Release | Highlights |
|---|---|
| **v0.4 → v0.26.x — foundation** | Everything the runtime is built on, condensed: **six providers** (Anthropic / OpenAI / DeepSeek / Gemini / Ollama cloud + local) behind one HTTP `Provider` interface + a synthetic **`code-js`** provider + a mock provider; the hardened model→tool_use→tool_result loop; **19 built-in tools** (Claude Code parity — Read/Write/Edit/Grep/Glob/NotebookEdit — plus HTTP/WebFetch/WebSearch/Bash/Agent/Skill/Memory/Channel/AgentDef/SkillDef/Evaluation/Interruption/Context); the content-addressed, runtime-mutable **substrate** (Agent / Skill / MCPServer / Schedule / Webhook / MemoryBackend / A2A defs); **Vector Memory** (sqlite-vec / pgvector) + pluggable **MemoryBackend** + the memory layer; **MCP on both sides**; the **LLM Gateway** + OpenAI-compatible shims; **A2A** interop; input **webhooks**; ensemble-sync primitives (RFC S); OTEL + per-tenant fairness + **Pause / Resume / Snapshot** + multi-replica **HA**; per-run named credentials + tool-use hooks; **OSS multi-tenant authorization (RFC L, v0.17.0)** across the state *and* definition planes; the embedded React **Web UI** + the **interactive terminal**; TS + Python + n8n adapters; Homebrew + Docker distribution. Per-version detail: [`REVISIONS.md`](REVISIONS.md). |
| **v0.27.0** | Interactive runs **survive leaving the terminal** — background-goroutine execution under `context.WithoutCancel` + re-attach via `GET /v1/runs/{run_id}/stream` (replay-from-`?from_seq` + live-tail); `Context op=self` reports the resolved provider + model |
| **v0.28.0** | Per-agent LLM **`sampling`** (temperature / top_p / top_k / penalties / seed / stop — yaml + AgentDef overlay + per-run) and **`pause` cooperative quiesce** — the loop parks at an iteration boundary and `Pause()` waits for in-flight runs, so a mid-run snapshot is reliable |
| **v0.29.0** | Web UI + operability — agent-editor sampling controls + a collapsible advanced JSON/YAML overlay, terminal message-echo + a context-size gauge, and **soft reclaim** of a retired agent name (no new runtime primitives) |
| **v0.30.0** | **Cross-instance resume of a snapshotted mid-run** (RFC X Phase 2) — a paused run is re-dispatched by reconstructing its loop from the transcript, fired after a snapshot restore and at boot (crash recovery, cluster-gated) |
| **v0.31.0** | Park + resume a **fan-out parent** blocked in `Agent.parallel_spawn` (RFC X Phase 3) — a pause-watcher + a no-schema-change spawn ledger, gated behind `LOOMCYCLE_RESUME_FANOUT` |
| **v0.32.0** | **Context-compaction subsystem** — replace older turns with a summary + keep-last-N verbatim (clean user-turn boundary, non-destructive); manual / auto / self triggers + a per-agent `compaction` block that flows down the spawn tree |
| **v0.33.0** | **External fan-out + the run-mutation surface on every transport** — `POST /v1/runs:batch` + a `spawn_runs` MCP tool (≤32 server-concurrent) + a `SpawnRunBatch` RPC; `compact_run` / `CompactRun`; per-run sampling + compaction on MCP/gRPC; `@loomcycle/client` 0.33.0 |
| **v0.34.0** | **Context-transform plugins** (RFC Z Phase 1a — the `redact` outbound-secret-scrub plugin), an exp7 self-review hardening pass, and a cross-provider thinking-model fallback **downgrade** (`reasoner`→`chat`) |
| **v0.34.1** | **Hardening + branding** (no new features) — a central tenant-scoping store accessor closing three live cross-tenant read gaps (security review S2) + the new loomcycle **brand logo** and favicon in the Web UI |
| **v0.34.2** | **Web UI design system + theming** — a tokenized `--lc-*` design system (spacing / type / radius / shadow / fonts + semantic colors), **light + dark themes** (OS default + a persistent topbar toggle), bundled brand fonts (Outfit / Inter / JetBrains Mono), and the **loom-wood `#56c596`** accent; plus a loop fix (interactive runs unbounded by default — no more stop-at-16) and a `-race` test de-flake |

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

## Quick start (seconds, authenticated)

```sh
loomcycle init --with-token   # writes config + mints a token to ~/.config/loomcycle/auth.env (0600)
export ANTHROPIC_API_KEY=sk-...   # (or OPENAI_API_KEY / DEEPSEEK_API_KEY) — at least one provider key
loomcycle doctor              # verify env + keys + storage + the just-minted token
loomcycle                     # starts on 127.0.0.1:8787 (auto-loads auth.env — no shell-rc edit)
```

`init --with-token` prints the Web UI URL (`http://127.0.0.1:8787/ui`); open it, then paste the token from `~/.config/loomcycle/auth.env` at the login prompt. (The token is kept in the `0600` file and never embedded in a URL — a `?token=` link would leak the bearer into browser history and any fronting proxy's logs.) `loomcycle` and `loomcycle doctor` both auto-load `auth.env` from the config dir; a real `export LOOMCYCLE_AUTH_TOKEN=…` always overrides it.

## Bootstrap tiers

Pick the tier that fits — each is a superset of the one above. **Auth is enforced only once something is configured**, so Tier 1 needs no token at all.

### Tier 1 — zero-config dev (open mode, localhost)

No token, no flags. Fastest way to kick the tires on `127.0.0.1`.

```sh
loomcycle init               # config only — no secret written
export ANTHROPIC_API_KEY=sk-...
loomcycle                    # open mode: /v1/* + /ui pass through unauthenticated (logs a warning)
open http://127.0.0.1:8787/ui
```

With no `LOOMCYCLE_AUTH_TOKEN` and no minted tokens, the runtime runs **open** on localhost — every request is allowed, whoami returns a synthetic admin. Great for a 10-second smoke test; **never** expose this off localhost.

### Tier 2 — single shared token (the recommended default)

One bearer gates everything. `init --with-token` is the easy button (above). Equivalent manual setup:

```sh
loomcycle init
export LOOMCYCLE_AUTH_TOKEN=$(openssl rand -hex 32)   # or: loomcycle init --with-token
export ANTHROPIC_API_KEY=sk-...
loomcycle
open "http://127.0.0.1:8787/ui?token=$LOOMCYCLE_AUTH_TOKEN"   # sets the cookie once
```

Treat anyone holding the token as fully trusted to drive the runtime.

### Tier 3 — multi-tenant, per-principal tokens (RFC L, v0.17.0)

Mint a distinct bearer per developer/app, each bound to an authoritative `(tenant, subject, scopes)`. Migrate a Tier-2 deployment in place — no downtime:

```sh
# promote your existing shared token into the substrate, then mint scoped tokens
loomcycle operator-token create --copy-from-env --name ops --tenant ops --scopes substrate:admin
loomcycle operator-token create --name acme-app --tenant acme --subject alice --scopes runs:create
```

The first admin `OperatorTokenDef` disables the legacy shared-token fallback. Per-route HTTP + per-RPC gRPC scopes; the Web UI becomes role-aware (super-admin vs tenant). See `Context.help operator-tokens` and the v0.17.0 notes in [`REVISIONS.md`](REVISIONS.md).

**Smoke any tier:**

```sh
curl http://127.0.0.1:8787/healthz
# {"ok":true}
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

**v0.34.2 — Web UI design system + light/dark theming.** No runtime primitives; a Web UI design pass + two small fixes. **(1) Tokenized design system:** a new `--lc-*` token layer (`web/src/tokens.css`) mirroring the brand design system — spacing / type-scale / radius / shadow / fonts + semantic colors. The legacy `--bg` / `--fg` / `--accent` / … names become aliases of the themed tokens, so the whole stylesheet themes for free. **(2) Light + dark themes:** default follows the OS `prefers-color-scheme`; a persistent topbar sun/moon toggle overrides it (localStorage), with a pre-paint script (no flash-of-wrong-theme). Dark is the current palette verbatim; light ships as a functional basic-neutral palette (the brand-cream refinement is the next step). **(3) Brand fonts + accent:** self-hosted, bundled Outfit (display) / Inter (body) / JetBrains Mono (code) — no CDN, embedded, offline-safe — and the accent moves from light-blue `#5b9dff` to the loom-wood brand green **`#56c596`** everywhere (CSS + charts). Form controls + the Activity charts theme against the tokens, and the topbar wordmark swaps per theme (near-white on dark, black-ink on light). **(4) Fixes:** interactive runs are now **unbounded by default** (an interactive terminal no longer stops after 16 turns with `max_iterations` — the runaway guard was never meant for an operator-driven, Cancel-bounded session; an explicit `max_iterations` is still honored), the `TestSchedulerBearerCompound` `-race` load-flake is fixed at the root (the 310-scale load test is capped under `-race`), and the agent editor's **advanced (raw overlay) now round-trips visibly** — it pre-fills from the source on reopen instead of starting empty (the overlay was always persisted; the empty box just made it look unsaved). Web-UI/CI only; no `@loomcycle/client` bump.

**v0.34.1 — Hardening + branding (no new features).** A security-hardening + cosmetic release on the road to v1.0. **(1) Central tenant-scoping (security review S2):** the per-handler tenant-isolation convention (`tenantVisible` / `sessionOwnershipOK`) becomes a single choke-point — a per-request **`tenantScopedStore`** accessor that folds a cross-tenant row into an opaque `*store.ErrNotFound` (no existence oracle) — and three **live cross-tenant read gaps** are closed: the run-scoped interrupt list (`GET /v1/runs/{id}/interrupts`), the user interrupt inbox (`GET /v1/users/{id}/interrupts`, via a new `tenantID` arg on `store.InterruptListByUser`), and the user run-state stream (`GET /v1/users/{id}/agents/stream`, via a `TenantID` on the run-state event + a filter). Per-user channel routes are gated to the principal's own subject. Whole-tenant model preserved (same-tenant subjects collaborate; super-admin sees all). **(2) New brand identity in the Web UI:** the topbar shows the new loomcycle **wordmark logo** (top-left; the dark-theme variant — wordmark recoloured to the theme foreground, loom-mark colours kept) and the new **favicon**. Runtime change is server-side only; no `@loomcycle/client` bump.

**v0.34.0 — Context-transform plugins + exp7 (v0.33.0 re-run) hardening + a cross-provider thinking-fallback fix.** One new primitive, one hardening line, one fix line. **(1) Context-transform plugins (RFC Z Phase 1a):** a runtime-wide plugin chain that sits between the agent's assembled context and the outbound LLM request, transforming a **copy** (deterministic, copy-on-write, the caller's history is never mutated; the synthetic `code-js` provider is exempt so replay stays byte-stable). Phase 1a ships the **`redact`** plugin — outbound secret scrubbing that reuses the F32 `redact.Redactor` (Tier-A exact env-value masking + Tier-B heuristic patterns: `Authorization` / `sk-` / `AKIA` / `xox` / `ghp_` / `key=value`), so a model never sees a configured secret even when it leaks into history. Configured via a top-level `context_plugins:` block. **(2) exp7 self-review re-run hardening:** every finding from a 10-agent fan-out review was independently verified against `main` (≈9 of ~40 refuted on verification, incl. a Go-1.24 `crypto/rand`-never-errors catch); the confirmed set landed as correctness/robustness fixes — admin POST `MaxBytesReader` caps, `ExportPretty` checksum, an OAuth-refresher `Stop()`-before-`Start()` deadlock, a `HeartbeatRunner` cancel race, a bounded backplane publish in `Bus.Notify`, scheduler `on_complete` hooks on the survival ctx, pause `Glob`/`Grep` idempotency, an absolute-in-root `Glob` pattern (R1), evaluation-`dimensions` parse logging, cmd shutdown-budget + SSE-safe `IdleTimeout` — plus a dead-code/cosmetic cleanup sweep. **(3) R2 — cross-provider thinking-model fallback downgrade:** a DeepSeek thinking model (`deepseek-reasoner` / `*-pro`) 400s ("reasoning_content must be passed back") when a fallback hands it a history whose assistant turns lack `reasoning_content` (a foreign provider produced them, or the reasoning strip zeroed them). A new optional `providers.ThinkingDowngrader` lets the loop downgrade to the non-thinking sibling (`reasoner`→`chat`, `*-pro`→`*-flash`) for that leg and emit a new `model_downgraded` event. **(4) `@loomcycle/client` 0.34.0:** version-aligned lockstep release, no client-surface change (the new event passes through the generic stream; context plugins are server config).

**v0.33.0 — External fan-out + the run-mutation surface on every transport + exp7 hardening.** Three feature lines and a self-review fix line. **(1) RFC Y external fan-out:** **`POST /v1/runs:batch`** + a **`spawn_runs`** MCP tool (mode `"join"`) spawn up to 32 fresh runs **server-side concurrent** in one call — bounded by the existing per-user admission gate, returning a combined **index-aligned** envelope once all settle; a per-child failure rides in-envelope (`status` + `error`), never failing the batch; the batch caller's authoritative tenant stamps every child. Replaces the "fire N serializing `spawn_run` calls" / in-loomcycle-dispatcher workaround (`mode:"detach"` awaits RFC P; `timeout_ms` caps the join). **(2) Compaction + sampling on every transport:** a **`compact_run`** MCP tool + a **`CompactRun`** gRPC RPC (the `POST /v1/runs/{id}/compact` op lifted to a shared `connector.CompactRun`; HTTP byte-identical), and per-run **`sampling`** + **`compaction`** overrides now on `spawn_run`/`spawn_runs` (MCP), the gRPC `RunRequest`/`ContinueRequest` (proto3 `optional` — `temperature: 0` stays deterministic), and `@loomcycle/client`'s `runStreaming`/`continueSession`. **(3) `@loomcycle/client` 0.33.0:** new `spawnRunBatch()` + `compactRun()` + per-run sampling/compaction (52 → 54 methods). **(4) exp7 self-review:** tenant-scope `DynamicAgentDelete` (cross-tenant delete), infra-secret YAML-expand deny + newline reject, `/v1/runs/{id}/input` scope-gate, scheduler error logging, an O(1) `ChannelGet`, an additive snapshot checksum, + three smaller hardening fixes.

**v0.32.0 — Context compaction subsystem (keep-last-N, auto-compact, per-agent settings + spawn inheritance).** A long session crowds the model's window; compaction summarizes older turns and continues from the summary. A compaction replaces the loop's in-memory history with `[pinned task? + summary, ack] ++ last-N kept verbatim`, snapped to a clean user-turn boundary so a tool_use/tool_result pair is never split; a `context_compaction` marker records summary + keep-N/keep-first so `replayTranscript` rebuilds the identical form on resume (full transcript retained — non-destructive). Four triggers, one shared summarizer (`loop.Summarize`): **manual** (the `/run` Compact button → `POST /v1/runs/{id}/compact`), **auto** (when `compaction.enabled` + the footprint crosses `autocompact_at_pct`, the loop compacts inline at a boundary — works for **autonomous** runs too, off by default, self-debouncing), **self** (`Context op=compact`), all emitting the marker + an OTEL `context.compaction` span event. The per-agent **`compaction`** block (mirrors `sampling`: yaml / AgentDef overlay / per-run / content-identifying / `Context op=self`) — `enabled`, `target_percentage` (10–50), `keep_last_n`, `keep_first`, `autocompact_at_pct` (50–95), optional cheaper summary `model` — **flows down the spawn tree** (child inherits the parent's effective policy, its own def fills the gaps, the parent overrides per-spawn via the Agent tool). Bundles the `/run` composer restyle (#458), the ✕ Stop button (#459), and the manual Compact button (#460). Runtime-only; no `@loomcycle/client` bump.

**v0.31.0 — Park + resume a fan-out parent blocked in `parallel_spawn` (F42 / RFC X Phase 3).** Closes the documented Phase-2 deferral: a run blocked inside `Agent.parallel_spawn` → `wg.Wait()` is *inside a tool call*, not at the loop's only park point, so on pause it never parked — `paused_runs_count` excluded it, the pause Manager warned "fan-out PARENT … did not reach a pause boundary", and a mid-fan-out snapshot missed the parent. v0.31.0 fixes both sides, **gated behind `LOOMCYCLE_RESUME_FANOUT` (default OFF → existing behavior byte-identical until opt-in)**: **(1) Capture** — a pause-watcher goroutine in `executeParallelSpawn` calls the existing `PauseGate.Park` on pause and unparks on resume, **without touching `wg.Wait`/result collection**, so the parent now counts as parked (warning gone, count accurate) and a same-instance pause→resume mid-fan-out just works. **(2) Durability** — a two-event **spawn ledger** on the parent transcript (`spawn_child_started` = index→run_id; `spawn_child_result` = a child that finished pre-snapshot), riding the existing event emitter, **no schema change**, ignored by `replayTranscript`. **(3) Resume** — `resumePausedRun` detects the parked fan-out parent and, in its background goroutine before taking a run slot (no semaphore deadlock), reconciles each child (durable ledger result, else **awaits** the re-dispatched child to terminal + reads its transcript), synthesizes the byte-compatible `{"results":[...]}` envelope, and seeds it into `PriorMessages` so the loop continues. Edges: pre-snapshot completion, gone agent (error result), never-dispatched-past-cap (re-issuable error), depth>1. Deferred: a mixed tool turn (parked fan-out + sibling in-flight tools) is flagged for manual re-attach; default-ON awaits exp6.5 re-validation. Runtime-only; no `@loomcycle/client` bump.

**v0.30.0 — Cross-instance resume of a snapshotted mid-run (F42 / RFC X Phase 2).** v0.28.0 made `pause` quiesce in-flight runs (Phase 1) so a mid-run snapshot is reliable; but a snapshotted `pause_state='paused'` run restored on another instance was **data only** — nothing relaunched its loop (`POST /v1/_resume` → `409 not_paused`; a restart didn't relaunch it). v0.30.0 **re-dispatches paused runs by reconstructing their loop from the transcript**: `ResumePausedRuns` re-resolves the agent, replays the transcript into `PriorMessages`, flips the row to `running`, re-registers cancel/pause/steer, and re-enters `loop.Run` under the **existing run_id** in a background goroutine — firing both after a **snapshot restore** (response reports `paused_runs_resumed`) and at **boot** (crash recovery; cluster-gated by an advisory lock so one replica resurrects each run). A new additive `runs.interactive` column (captured + restored) preserves park-vs-complete semantics, and the stale-run sweeper now skips parked (`paused`/`pausing`) runs so they aren't killed before resume. Limitations: per-run secrets (`user_bearer`/credentials) and call-time overrides (allowed_hosts/sampling/metadata) aren't snapshotted (re-derived from the agent def); a run idle-awaiting-input when paused is flagged for manual re-attach, not auto-resumed. Runtime-only; no `@loomcycle/client` bump.

**v0.29.1 — Patch: adapter lockstep publish.** The additive `max_context_tokens` usage-event field (v0.29.0) shipped in the runtime but the `@loomcycle/client` npm publish skipped — the publish workflow only fires when the adapter's `package.json` version equals the release tag, and it was at `0.26.0`. v0.29.1 realigns the adapter version with the tag so `@loomcycle/client@0.29.1` publishes with the field. No runtime change (binaries identical to v0.29.0).

**v0.29.0 — Web UI + operability: agent-editor controls, terminal UX, soft reclaim.** Three operator-facing improvements (no new runtime primitives). **(1) Agent editor** (#449): dedicated **sampling** controls (temperature/top_p/top_k/penalties/seed/stop — string-typed so blank = unset, `0` = explicit) plus a collapsible **advanced JSON/YAML overlay** for the long tail (channels, interruption, `*_def_scopes`, …) the substrate already accepts; empty box never blocks, a malformed non-empty body blocks inline. **(2) Interactive terminal** (#450): your own messages now echo into the transcript (`❯ …` — initial prompt, steer, continue; they were filtered from the live tail), plus a **context-size gauge** `ctx 47.2k / 200k (24%)` backed by an additive `max_context_tokens` on the usage event (`@loomcycle/client` publishes the field in **0.29.1** — the v0.29.0 adapter publish skipped on a version mismatch; gRPC parity is a fast-follow). **(3) Soft reclaim** (#452): retiring the active agent now clears the active pointer so the **name is reclaimable** (recreate to grant more tools — a fork can't widen the `allowed_tools` ceiling, a fresh create can); also fixes a latent bug where a retired-but-active def was still served to runs; the Library badges inactive/retired names. No hard delete — full audit lineage preserved. Plus a bundled `examples/exp6-self-evolving-agents/` (#451); `allowed_hosts` per-run narrowing already lives in the run launcher (caller-authoritative).

**v0.28.0 — Per-agent LLM sampling + `pause` actually quiesces.** Two features. **(1) Per-agent sampling:** a grouped `sampling:` block (`temperature`, `top_p`, `top_k`, `frequency_penalty`, `presence_penalty`, `seed`, `stop`) settable via static yaml, the AgentDef substrate (create/fork — how a self-evolving breeder mints variants), and per-instance on `POST /v1/runs`; overlays/overrides merge **per field** (per-run > per-agent > provider default; `temperature: 0.0` is deterministic, ≠ unset). Each driver maps what its provider supports (Anthropic drops temperature when `effort` engages thinking); content-identifying; reported back via `Context op=self`. **(2) `pause` cooperative quiesce (F41/RFC X):** the run loop now parks at a clean iteration boundary, `Pause()` waits (up to `timeout_ms`) for in-flight runs to park so `paused_runs_count` is accurate, and new runs are 503-gated (`/v1/runs` + gRPC + webhook/A2A) — making "pause + snapshot a mid-run" reliable (a fan-out parent mid-`parallel_spawn` is the documented Phase-2 deferral). Runtime-only; no `@loomcycle/client` bump.

**v0.27.2 — Patch: collapsed tool results actually fold.** The collapsed `tool_result` summary used `oneLine(text)` (flattens whitespace, doesn't truncate), so a large result showed in full whether folded or not. It's now truncated to a 100-char summary like `tool_call`; the full output stays in the expand. Frontend-only.

**v0.27.1 — Patch: interactive-terminal Web UI polish.** Three follow-ups to the v0.27.0 terminal: the transcript now **auto-scrolls** to follow live output (streaming text coalesces into one line, so the old line-count trigger stalled the tail; it now follows the `events` stream with a stick-to-bottom ref that pauses when you scroll up); **`tool_call` collapses** to a one-line summary + expand-on-click (a `Write`'s full file body no longer floods the scrollback), matching `tool_result`; and the continue/steer box is a **multi-line `<textarea>`** — Enter sends, Shift+Enter inserts a newline, auto-growing up to a cap. Frontend-only; no `@loomcycle/client` bump.

**v0.27.0 — Interactive runs survive leaving the terminal, and you can come back.** v0.26.x's interactive `/run` run was bound to its HTTP request — navigating to the runs menu closed the SSE stream, cancelled the request ctx, and killed the parked run. v0.27.0 runs an interactive run in a **background goroutine** under `context.WithoutCancel` (keeps the request's auth/tenant values, isn't cancelled on disconnect; stops only via the cancel registry), and adds **`GET /v1/runs/{run_id}/stream`** to **re-attach** (replay from `?from_seq` + live-tail, re-emitting stored events as SSE frames; `ScopeRunsRead` + tenant-ownership gated). A new run-scoped incremental store read (`GetRunEventsSince`, sqlite + postgres) backs the tail; a **"resume in terminal"** link on a running agent in the runs list is the way back. Two more changes ride along: **`Context op=self`** now returns the resolved **`provider` + `model`** (non-secret introspection, stamped per-iteration so it's fallback-truthful), and the terminal UI caps its height (inner-scroll instead of growing the page) and **collapses tool results** (one-line summary + expand-on-click; operator/agent messages stay full). Behaviour note: the steer *echo* frame is no longer on the live wire for interactive runs (steering itself is unchanged). Single-replica re-attach (DB-backed deployments work cluster-wide since events are in the shared store); cross-replica + multi-viewer deferred. Adds one GET route; no `@loomcycle/client` bump.

**v0.4 → v0.26.x — foundation and substrate build-out (squashed).** The primitives the runtime is built on, in one line: six provider drivers (Anthropic / OpenAI / DeepSeek / Gemini / Ollama cloud + local) behind one `Provider` interface with per-tier + effort routing and fallback, plus the synthetic `code-js` provider and a mock provider; the hardened model→tool_use→tool_result loop; the 19 built-in tools; the content-addressed, runtime-mutable substrate (Agent / Skill / MCPServer / Schedule / Webhook / MemoryBackend / A2A defs, verify-or-fork across deployments); Vector Memory (sqlite-vec / pgvector) + pluggable MemoryBackend + the memory layer; MCP on both sides; the LLM Gateway + OpenAI-compatible shims; A2A interop; input webhooks; the RFC S ensemble-synchronization primitives; OTEL + per-tenant fairness + Pause / Resume / Snapshot + multi-replica HA; per-run named credentials + tool-use hooks; **OSS multi-tenant authorization (RFC L, v0.17.0)** across both the state and the definition planes; the embedded React Web UI and the interactive terminal (through v0.26.x); the TS + Python + n8n adapters; Homebrew + Docker distribution. Full per-version detail for all of the above is in [`REVISIONS.md`](REVISIONS.md).

**Planned — v1.0 (hardening + distribution):**

**No new features are planned before v1.0.** The feature set is complete — the remaining work is **finishing the in-progress test / QA + security-hardening pass** across the shipped surfaces; once that's green, the **v1.0** tag ships. No new primitives, no new wire surface — every change from here is a fix, a test, or distribution polish. (With the multi-tenant-auth capstone shipped in v0.17.0, v1.0 is a pure hardening + distribution milestone.)

- **Distribution + bootstrapping.** A first-run install story that survives contact with a fresh machine: hardened **Homebrew** formula and **Docker** images (multi-arch, the `init` / `doctor` flow, a sane default config), so `brew install` / `docker run` gets an operator to a working sidecar without reading the docs first.
- **Claude Code plugin hardening.** The `claude-code-plugin-loomcycle` plugin (slash commands + skills + hooks over `loomcycle mcp`) gets a robustness pass — error surfaces, version-skew handling, and a clean bootstrap from the published binary.
- **Security + robustness + runtime-QA pass** across the v0.13–v0.17 surfaces (A2A, webhooks, pluggable memory + the memory layer, the code-js provider, the multi-tenant-auth substrate), then the **v1.0** tag.
- **Enterprise auth** (SSO / RBAC / SCIM / signed, queryable audit) stays out of scope for OSS — it's a separate edition built on the same `OperatorTokenDef` substrate.
- **Beyond** (polish): a settings UI, an operator cookbook of postures, broader distribution (Helm).

Full per-version release notes: [`REVISIONS.md`](REVISIONS.md).
Public roadmap with v0.8.x → v1.0 design details: [`docs/PLAN.md`](docs/PLAN.md).

## Architecture

Three diagrams cover different views of the same runtime:

<p align="center">
  <img src="docs/assets/architecture.png" alt="loomcycle architecture — clients at the top (app servers, CLIs, TS/Python SDKs, Claude Code & MCP orchestrators, LangChain/n8n via OpenAI-compat shim), the single Go binary in the middle (1..N replicas; five wire surfaces incl. HTTP+SSE / gRPC / Web UI / MCP server with 40 meta-tools / LLM Gateway → bearer auth + concurrency semaphore + per-user fairness → 36-method connector.Connector → agent loop → tool dispatcher with 19 built-in tools + MCP client transport + sub-agent runner → SQLite/Postgres store covering sessions, runs, events, memory, channels, substrate tables, replicas+user_quotas+runtime_state+hooks), OpenTelemetry sidecar emitting spans, and external services at the bottom (seven LLM providers including anthropic-oauth-dev, three embedders, external MCP servers cloud)" width="780" />
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
- [`docs/CLAUDE-CODE.md`](docs/CLAUDE-CODE.md) — driving loomcycle from Claude Code: the recommended `claude-code-plugin-loomcycle` plugin (slash commands + skills + hooks) vs. the manual `loomcycle mcp install` path.
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
