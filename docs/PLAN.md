# Roadmap

This is the public roadmap. For decision history, regret notes, and per-version commit-by-commit details, see `doc-internal/PLAN.md` (gitignored).

## v0.8.4 — current

**Status: shipped (2026-05-11).** The **Channel tool** — persistent inter-agent message bus. One agent publishes JSON payloads to a named channel; another subscribes and drains them with cursor-based at-least-once delivery. Closes the gap between Memory (state) and Agent (RPC spawn) — channels are the asynchronous decoupled handoff primitive. Framework primitive #3 (after Memory v0.8.0 and user_tier v0.8.2) on the way to the LoomCycle MCP capstone.

**What's in v0.8.4 (vs v0.8.3):**

- **`Channel` built-in tool** with five ops: `publish` / `subscribe` / `ack` / `peek` / `list_channels`. Single discriminated `op` field, same shape as Memory. Storage-layered design: messages persist to the existing `store.Store` (sqlite + postgres) via two new tables (`channel_messages` + `channel_cursors`); same-process subscribers waiting in long-poll mode get sub-millisecond notification via an in-process `Bus`. Cross-process notification (multi-replica deployments) deferred to v0.9.x — single-replica is today's only deployment.
- **Cursor scope mirrors Memory's** (`agent` / `user` / `global`). Two researcher-agent runs share a cursor on the same `agent`-scoped channel — that's the queue semantic. Two different agents subscribing to the same channel each maintain their own cursor — that's the work-distribution shape. `global` scope is the cross-tenant fan-out option (one shared cursor for the whole channel; no automatic isolation, operator declares the channel explicitly).
- **At-most-once-by-default for `subscribe`; at-least-once via `peek` + `ack`.** Subscribe commits `next_cursor` BEFORE returning (commit-on-return — agents looping on subscribe march forward without tracking cursors). Agents needing crash safety call `peek` (non-consuming) → process → `ack` (explicit cursor commit). Cursor monotonicity enforced — `ChannelAck` rejects cursor values older than the currently committed one (`ErrChannelCursorRegression`) so buggy agents can't rewind delivery. Two-step peek+ack matches SQS / Kafka consumer idioms.
- **Operator-yaml ACL.** Channels MUST be declared in the top-level `channels:` block; agents grant themselves access via per-agent `channels: {publish: [list], subscribe: [list]}`. Wildcards (`findings/*`) are prefix-anchored — match `findings/alpha` but NOT `findings`. Mid-string globs (`*`) are rejected at config load. Sub-agents inherit the parent's ACL via ctx, same shape as `WithMemoryPolicy` and `WithHostPolicy`.
- **Long-poll subscribe.** Optional `wait_ms` budget on a subscribe; if the storage read returns empty, the tool blocks on the in-process `Bus.Wait` for that channel until either a new publish lands or the timeout fires. Cap is operator-controlled (`LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS`, default 30 s) so a hung subscribe doesn't leak goroutines on agent crash.
- **Bounded storage with lossy-on-overflow.** Each channel declares `max_messages`; publishes that would push the per-(channel, scope, scope_id) count past this trim the OLDEST rows inside the same txn. Publisher never blocks. 0 = unbounded. Sweeper goroutine (mirror of Memory's) keeps the table bounded over time; read paths filter expired rows at WHERE regardless of sweeper cadence.
- **Three new env vars:** `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES` (per-publish payload cap; default 64 KB), `LOOMCYCLE_CHANNELS_SWEEP_MS` (TTL reaper cadence; default 15 min), `LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS` (max wait_ms; default 30 s). All have sensible defaults; zero disables.
- **Test coverage:** 11 storetest contract tests + 7 bus race-detector tests + 10 tool-layer tests. SQLite and Postgres both validated against the same contract (cursor monotonicity, TTL filter at read, max_messages trim, scope isolation, replay via `cur_0`, ack-regression rejection).
- **Operator visibility:** Boot log emits `channels: configured N — channel-a / channel-b / ...` (mirror of `user_tiers:` line). Sweeper logs per-sweep delete count.

**Architecture decisions (resolved in the v0.8.4 RFC):**

- **Storage-layered backend** (not Postgres `LISTEN/NOTIFY`, not Redis Streams). Operators get no new infra dependency; sqlite + postgres parity matches Memory's shape. Cross-process notification — if v0.9.x multi-replica HA proves dominant — can swap `bus.go`'s backplane without changing the tool surface.
- **Cursor scope = Memory scope.** One mental model, one isolation story, one set of operator-yaml allowlists to maintain. The cost is that `global` scope channels need careful operator review (one bad ACL can leak across tenants); this is documented inline in `loomcycle.example.yaml`.
- **No DLQ in v0.8.4.** Readers that abandon a cursor leave messages until TTL; operators debug via `peek` from `cur_0`. Real dead-letter-with-retry-count is a v0.9.x candidate.
- **Polling subscribe, not streaming.** Tool calls stay synchronous from the model's POV; real-time-ish behaviour comes from `wait_ms` + the in-process bus. Streaming subscribe would couple Channels to a long-running-tool-output plumbing that doesn't exist yet — separate concern.

Detailed design in `doc-internal/rfcs/channels-tool.md` (gitignored).

For the v0.8.3 baseline that drove this work, see [v0.8.3](#v083--earlier).

## v0.8.3 — earlier

**Status: shipped (2026-05-11).** Split the single `ollama` provider registration into two — `ollama` (hosted ollama.com, Bearer auth via `OLLAMA_API_KEY`) and `ollama-local` (local-network Ollama, no auth, `OLLAMA_BASE_URL`). One driver package serves both; the constructor takes `providerID` + `apiKey` and threads them through `ID()`, the rate-limit Config, the non-2xx error formatter (matters for v0.8.2's classifier's `"<name> <code>:"` prefix anchor), and the optional `Authorization: Bearer` header. Operator yaml referencing `provider: ollama` for a local backend renames to `provider: ollama-local`; existing deploys keep working unchanged because `OLLAMA_BASE_URL=http://localhost:11434` now feeds `ollama-local`. PR #55.

For the v0.8.2 baseline, see [v0.8.2](#v082--earlier).

## v0.8.2 — earlier

**Status: shipped (2026-05-11).** The **user_tier** feature — operator-defined per-user-tier provider/model policies that overlay resolver behaviour AND drive runtime fallback when a provider call hits a retryable error. Two-PR ship: PR #52 (resolve-time overlay + wire/store/yaml) + PR #53 (runtime fallback + error classification + typed events). Enables jobs-search-agent and similar consumers to surface differentiated user tiers (free / low / medium / high) with operator-defined cost/quality/privacy tradeoffs.

**What's in v0.8.2 (vs v0.8.1):**

- **Named user_tiers in operator yaml** (PR #52). New top-level `user_tiers:` map with named entries (`default` required; common shape `default` + `free` + `low` + `medium` + `high`). Each entry carries `provider_priority`, per-task-tier `tiers` (low/middle/high candidate lists), `fallback_on_error` switch, and `max_fallback_attempts` cap. The "default" entry is required when the block is populated — covers v0.7.x clients that don't yet send `user_tier` in the request body.
- **Wire protocol** (PR #52). `POST /v1/runs` and `POST /v1/sessions/{id}/messages` both accept an optional `user_tier` field. Empty falls through to `cfg.UserTiers["default"]`; unknown name → 400 with a clear error. Sub-agents inherit the parent's `user_tier` via ctx so the whole sub-run tree uses one tier policy.
- **Resolver overlay precedence** (PR #52). The overlay sits BETWEEN library defaults and per-agent overrides: `library < user_tier < per-agent`. When per-agent `providers:` AND `user_tier.provider_priority` are both set, the resolver walks the intersection (in per-agent order, preserving operator intent within the tier-restricted space). **Empty intersection → `ErrTierAgentNotAvailable`** — operator policy refusal distinct from a transient outage, so clients render "upgrade required" instead of retrying. Same refusal also fires when `agent.Models[tier]` lists no candidates whose provider is in the user_tier's priority.
- **Runtime provider fallback** (PR #53). When a provider call returns a retryable error AND the tier's `fallback_on_error: true` AND the cumulative attempt budget isn't exhausted, the loop swaps to the next-in-queue provider in the tier's candidate list and continues the iteration. The Call-layer error path AND the in-stream `EventError` path both trigger fallback (the latter handles mid-stream 5xx). The MarkStalled signal still fires so the resolver matrix knows to skip the failed pair on subsequent picks.
- **Error classification taxonomy** (PR #53). New `internal/providers/errclass.go` with 5-bucket `ClassifyError`: **Retryable** (429, 500/502/503/504, network DNS/conn-refused, v0.8.1 stream-idle) → fallback; **Permanent** (400, 401, 403, 422, other 4xx) → propagate (cascading would burn through every provider's quota for a config issue); **Cancelled** (`context.Canceled`) → propagate (caller abandon); **DeadlineExceeded** (`context.DeadlineExceeded` on the root ctx, NOT v0.8.1 stream-idle) → propagate. Priority-of-checks invariant: v0.8.1 stream-idle marker beats the generic DeadlineExceeded branch even though the wrap chain satisfies `errors.Is(..., DeadlineExceeded)` — wrong here would misclassify every stream-idle as caller-deadline and lose the retry.
- **Cumulative 3-attempt budget** (PR #53). `MaxFallbackAttempts` defaults to 3 (cap on cumulative provider switches per run; the original provider doesn't count). Prevents runaway-fallback loops under backbone-wide outages while still allowing recovery from single-provider issues. 0 in yaml falls through to the package default; negative is rejected at config-load.
- **Per-tier `fallback_on_error` switchability** (PR #53). Free tier's `fallback_on_error: false` makes a 429 return an error to the client — no climb to paid providers — preserving the cost cap. Paid tiers' `fallback_on_error: true` enables the cascade. The "default" tier defaults to `fallback_on_error: true` so back-compat v0.7.x clients keep the rate-limit retry behaviour they had.
- **Typed events** (PR #53). `EventProviderFallback` (with structured `FallbackInfo` payload — failed/new provider+model, attempt counter, user_tier name, error class, truncated cause) fires on every successful switch. `EventCacheInvalidated` fires when switching AWAY from Anthropic (the only provider with operator-controlled `cache_control` breakpoints today; downstream tokens on the new provider are cache-cold). Both are wire-stable; adapters/SSE consumers can string-match on `Type`.
- **Per-run audit marker** (PR #52). New `runs.user_tier TEXT` column on both SQLite + Postgres adapters via additive `0003_user_tier.up.sql` migration (no locking; safe to apply live). Compliance + cost-retrospective queries facet by tier without grepping logs. Boot log emits `user_tiers: configured N — default / free / low / medium / high` so operators see what's available at startup.

**Operator yaml shape:**

```yaml
user_tiers:
  default:                            # required
    provider_priority: [anthropic, deepseek]
    tiers:
      low:    [{provider: anthropic, model: claude-haiku-4-5}]
      middle: [{provider: anthropic, model: claude-sonnet-4-6}]
      high:   [{provider: anthropic, model: claude-sonnet-4-6}]
    fallback_on_error: true
    max_fallback_attempts: 3
  free:
    provider_priority: [gemini, ollama]
    tiers: { low: [...], middle: [...], high: [...] }
    fallback_on_error: false          # cost cap: 429 → error, no climb
  low:
    provider_priority: [gemini, deepseek]
    fallback_on_error: true
  medium:
    provider_priority: [deepseek, anthropic]
    fallback_on_error: true
  high:
    provider_priority: [anthropic]
    fallback_on_error: true
```

**Architecture decisions worth flagging:**

- **The role split is the architectural insight.** Operator-owned policy lives in `loomcycle.yaml`; application-owned identity lives in the request body. Loomcycle is the transport — it doesn't need to know what users pay for or how billing works. This mirrors the same operator/caller split that `allowed_hosts` already uses (operator floor in env, caller-authoritative per-request override).
- **Two refusal paths produce the same `ErrTierAgentNotAvailable`.** Path 1: per-agent `providers:` ∩ user_tier `provider_priority` is empty. Path 2: per-agent `Models[tier]` lists no candidates whose provider is in the user_tier's priority. Both surface the same typed error so clients render one consistent "upgrade required" UI.
- **The cumulative budget is the right shape for cost-fairness.** A free user can't escape the cap (`fallback_on_error: false`), and a paid user's runaway-fallback-loop is bounded at 3 attempts. Without the cumulative cap, a paid user hitting a backbone-wide outage could try every provider in their queue plus retry the original a few times, burning resources to no benefit.
- **Stream-idle priority over DeadlineExceeded.** v0.8.1's per-byte idle wrap surfaces as `"stream read: context deadline exceeded"` and the wrap chain satisfies `errors.Is(..., DeadlineExceeded)` — but the SEMANTIC is "provider stalled, another might be healthy" (retryable), not "caller's deadline cap hit" (propagate). The classifier checks the substring marker BEFORE the generic DeadlineExceeded branch. Pinned by `TestClassifyError_StreamIdleHasPriorityOverDeadlineExceeded`.
- **`EventCacheInvalidated` is intentionally narrow.** Only `anthropic → other` emits it. Gemini's implicit cache and DeepSeek's auto-cache aren't operator-controlled, so there's no operator-visible state to "lose" on switches involving them. If a future provider gains operator-controlled cache (or Gemini exposes one), the emission condition expands; the event type itself stays the same.

For the v0.8.1 baseline that drove this work, see [v0.8.1](#v081--earlier).

## v0.8.1 — earlier

**Status: shipped (2026-05-10).** Operational hardening: three production-readiness improvements that surfaced from the jobs-search-agent VM bring-up and dev-mac reproductions. None are new framework primitives (those resume in v0.8.3 with the Channel tool — v0.8.2 absorbed the `user_tier` work after v0.8.1 shipped); these three fix failure modes that made running loomcycle in a server environment painful.

**What's in v0.8.1 (vs v0.8.0):**

- **Provider streaming timeouts** (PR #47). Replaced the 5-min wall-clock `http.Client.Timeout` with two finer-grained ceilings: `Transport.ResponseHeaderTimeout` caps time-to-first-byte (default 60 s; per-driver Transport so one stalled provider doesn't starve another's connection reuse), and a body wrap resets a per-byte timer that cancels the request context on stall (default 90 s). Long but actively-emitting final-turn responses — e.g. job-searcher building a 25-position ingest payload — now complete instead of getting cut mid-stream. The wall-clock cap had been firing during the FIRST few seconds of the stream's body in some cases (job-searcher run `r_ea963a36bc` killed at 659 s with `provider error: stream read: context deadline exceeded` despite the model still emitting). New package `internal/providers/streamhttp/` with 8 unit tests covering active vs stalled streams, idempotent close, and the no-Client.Timeout sanity check; race-detector clean. All five drivers updated to `New(apiKey, baseURL, streamhttp.Options, *http.Client)`. Two new env vars: `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS`, `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS` (both clamped to sensible defaults; misconfigured negative/zero values fall through to the default).
- **Lazy MCP retry on first agent call** (PR #48). MCP servers that failed initial handshake at boot used to stay marked `skipped` for the lifetime of the loomcycle process (the boot-time retry budget is 30 s shared across all servers; once it expires, the dispatcher map is built without those tools). Operators had to restart loomcycle by hand once the peer recovered. Now `tools.Dispatcher` carries an optional `FallbackFunc`; a lookup miss for `mcp__<server>__<tool>` against a configured-but-skipped server triggers one fresh `pool.Get` for that server on the agent's call path, registers the tools in the resolver's memo, and dispatches. Subsequent calls hit the cache without re-handshaking. The pool's existing `entry/ready` channel coalesces concurrent first-touches to a single underlying handshake (50-way concurrency test pinned the regression guard). Operator-visible log line: `mcp[<server>]: lazy-registered N tool(s) on first agent call (was skipped at boot)`. Addresses the "components restart independently in a server environment" failure mode — peer restarts no longer cascade into loomcycle restarts.
- **Agent directory discovery** (PR #49). New `LOOMCYCLE_AGENTS_ROOT` points at a directory of flat `<name>.md` files. Each file's YAML frontmatter is parsed as the base `AgentDef`; the body becomes `system_prompt`. The yaml `agents:` map remains an OPTIONAL override layer — yaml entries with the same name override discovered fields per-field (yaml-as-override semantics; nil yaml field = absent, non-nil = explicit override that lets `allowed_tools: []` actively zero-out a discovered list). Mixed-mode, MDs-only, and yaml-only deployments all supported; existing yaml-only deploys are a strict regression guard. Frontmatter is flat top-level keys: `name` / `description` / `tools` / `model` / `tier` / `models` / `effort` / `max_tokens` / `skills` / `memory_scopes` / `memory_quota_bytes` / `providers` / `allowed_tools` / `system_prompt_file`. The `tools:` field accepts both Claude Code's comma-string (`tools: A, B, C`) and loomcycle's yaml list (`allowed_tools: [A, B, C]`); `allowed_tools` wins when both present. New `internal/agents/` package mirrors `internal/skills/` shape (same delimiter rules, same body-only fallback). Eliminates the synchronisation pain operators hit maintaining `.claude/agents/<name>.md` for Claude Code AND a corresponding loomcycle `agents:` block — single source of truth in normal operation, per-environment yaml overrides when needed.

**Operator env vars (new in v0.8.1):**

- `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS` (default 60000) — per-attempt cap on time-to-first-byte for streaming provider calls. Bump to 90000+ on networks with high TLS handshake latency or aggressive NAT idle timeouts.
- `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS` (default 90000) — max gap between body bytes during a streaming response. Bump if you see mid-stream `context deadline exceeded` errors on long final turns where the model is provably still emitting (reasoning-model thinking pauses on extended budgets can exceed the default).
- `LOOMCYCLE_AGENTS_ROOT` (unset by default) — directory of `<name>.md` files for agent-discovery. When unset, behaviour is unchanged from v0.8.0 (yaml-only).

**Architecture decisions worth flagging:**

- **The streaming-timeout pair replaces the wall-clock `Client.Timeout` everywhere.** No driver retains the old behaviour; the constructor signature change is uniform across anthropic / openai / deepseek / gemini / ollama. Tests covering the prior wall-clock semantic were updated, not preserved as compatibility guards — the prior behaviour was the bug.
- **Lazy MCP retry doesn't mutate `s.tools`.** Lazy-resolved tools live only in the `LazyResolver`'s memo; they're served by the `FallbackFunc`. Tools that need to be ADVERTISED in the model's spec list (i.e. discoverable by a fresh model with no prompt-side knowledge of the tool name) would still need boot-time registration. That's a v1.x concern; the v0.8.1 design assumes agent prompts already name the tools they call (the existing operator pattern).
- **Agent directory discovery uses yaml-as-override, not MDs-as-override.** Operators expressed the dev/main divergence pain shape: MDs are the natural editing surface (one file per agent, lives next to Claude Code's agent files), yaml is the per-deployment-tweak surface (override `max_tokens` on staging without editing the MD). Reverse semantics would force every per-environment tweak into a separate set of MD copies.
- **No strict-frontmatter enforcement (`yaml.KnownFields(true)`) yet.** Typo'd keys silently parse as zero values today. Mitigation: `.env.example` documents the canonical key list. A follow-up PR can tighten both the agents loader and the skills loader together.

For the v0.8.0 baseline that drove this work, see [v0.8.0](#v080--earlier).

## v0.8.0 — earlier

**Status: shipped (2026-05-09).** First v0.8.x point release: the **Memory tool** — persistent agent-scoped key/value storage that survives across runs and sessions. Five-op surface (`get` / `set` / `delete` / `list` / `incr`) over a new `memory` table on both SQLite and Postgres. Per-agent yaml gates which scopes an agent may use; operator env vars cap per-write and per-scope bytes. The first of four v0.8.x framework primitives sequenced toward the v1.0 LoomCycle MCP capstone.

**What's in v0.8.0 (vs v0.7.4):**

- **`Memory` built-in tool** (PR #45). The model invokes one tool with a discriminated `op` field; loomcycle resolves `scope_id` server-side from the run's identity (yaml agent name for `agent` scope; `user_id` for `user` scope) so a model-supplied scope_id can never read another user's keys. Atomic increment is the v0.8.0 counter primitive — concurrent same-key increments serialise via `BEGIN IMMEDIATE` on SQLite and `pg_advisory_xact_lock` on Postgres (a 100-goroutine regression test catches lost-update races on either backend). TTL is in seconds; expired entries are filtered at read time so agents never see stale values, with a periodic sweeper to keep the table bounded.
- **Per-agent yaml policy.** New fields: `memory_scopes: [agent, user]` (default-deny allowlist — `Memory` in `allowed_tools` is necessary but not sufficient) and `memory_quota_bytes` (per-agent override of the global `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` default). Sub-agents get THEIR OWN `memory_scopes` from yaml — the parent's policy does NOT cascade. This matches the existing `allowed_tools` model: a child's surface is its own yaml's authority. Cross-agent state-sharing is what the `user` scope is for.
- **Web UI Memory page** (`/ui/memory`). Three-pane browser: scope picker (`agent` / `user`) → scope_id list with key counts and byte totals → keys with prefix filter → entry detail with pretty-printed JSON, created_at / updated_at, and TTL. Polls the new `/v1/_memory/*` admin endpoints on a 5 s tick. Operators can audit what an agent has stored without dropping into SQL.
- **Admin API.** Four read-only routes — `GET /v1/_memory/scopes`, `/scopes/{scope}`, `/scopes/{scope}/{scope_id}/keys`, `/scopes/{scope}/{scope_id}/keys/{key...}`. Bearer-authed via the existing middleware; same admin posture as `/v1/_users` / `/v1/_resolver`. The `{key...}` multi-segment route handles the common `events/2026-05-09T10:00`-style key shape.
- **Pre-existing host-policy fix bundled in.** Code review of the v0.8.0 work surfaced that `handleMessages` (the session continuation path) had been missing `tools.WithHostPolicy` on its loop ctx since v0.4.0 — sub-agents spawned from a continuation fell back to the operator's static allowlist instead of the caller's narrowed list. Fixed alongside the new Memory ctx values; same shape as the v0.4.0 fix `9677b85` made for top-level runs.

**Operator env vars:**

- `LOOMCYCLE_MEMORY_MAX_VALUE_BYTES` (default 65536) — per-write cap on the `value` payload.
- `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` (default 1048576) — default per-(scope, scope_id) cap; per-agent yaml overrides this.
- `LOOMCYCLE_MEMORY_SWEEP_MS` (default 900000 / 15 min) — TTL reaper goroutine cadence; 0 disables.

**Architecture decisions worth flagging:**

- **`scope_id` is server-resolved, not model-supplied.** The model picks the SCOPE (agent vs user); loomcycle picks the SCOPE_ID from the run context. Non-negotiable.
- **No automatic eviction.** Quota exceeded → write fails with `quota_exceeded`. Operators set quotas deliberately; agents call `delete` explicitly. LRU is a v0.9.x candidate.
- **No encryption-at-rest.** Memory rows land in the same DB as transcripts (which already carry user prompts + tool outputs). Disk encryption is operator-config-wide, not Memory-specific. Revisit alongside v0.9.x HA work.
- **Postgres migration `0002_memory.up.sql` is additive-only.** New table with no locking on existing tables; safe to apply against a live database with zero downtime.
- **Concurrency primitive caught two real bugs at review time.** SQLite's `BeginTx(nil)` is DEFERRED (modernc/sqlite ignores `sql.LevelSerializable`); fix uses a pinned connection + raw `BEGIN IMMEDIATE` / `COMMIT`. Postgres `SELECT FOR UPDATE` doesn't lock absent rows; fix uses `pg_advisory_xact_lock(hashtextextended(key, 0))` so different keys don't contend. Both have a 100-goroutine regression test that demonstrably failed pre-fix.

For the v0.7.4 baseline that drove the v0.8.0 framework-primitive work, see [v0.7.4](#v074--earlier).

For the full Memory tool surface and yaml shape, see [TOOLS.md → Memory tool](TOOLS.md#the-memory-tool--persistent-agent-scoped-storage-v080).

## v0.7.4 — earlier

**Status: shipped (2026-05-09).** Iteration on top of v0.7.3 from the operator's first day running the Web UI against jobs-search-agent. Five PRs (#39 + #40 + #41 + #42, plus the in-process Gemini config-validation hotfix) addressing real UX gaps and a wire-shape drift the v0.7.3 ship missed. No breaking wire changes.

**What's in v0.7.4 (vs v0.7.3):**

- **Web UI agent name + content fixes** (PRs #41, #42, 2026-05-09). Two distinct bugs surfaced by running the UI end-to-end:
  - **Run list shows agent names.** v0.7.3 listed runs by `agent_id` UUID only — nothing identifying *which* agent each row was. Root cause: the wire response carried only the UUID; the YAML-declared name (`qa-agent`, `company-researcher`) lived on `sessions.agent`, not the run row. Fixed by JOIN-ing sessions in the SQL queries (denormalising onto `Run.Agent` at read time, matching the existing `UserID` denorm pattern), surfacing as `agent` in the JSON response, and updating the React `Agent` type to match. Both SQLite + Postgres adapters carry the JOIN.
  - **Transcript event panels render actual content.** v0.7.3's `AgentDetail` showed only the type label (`TEXT`, `TOOL`) on each event — no text, no tool params, no results. Root cause: the transcript endpoint nests the event payload under `event:` (alongside `seq` / `run_id` / `ts_ns` wrapper fields), but the React code was reading `ev.text` / `ev.tool_use` directly off the top level. Fixed types + reads.
  - Bonus: events render as **collapsed-by-default panels** with a one-line summary (first line of text, tool name + args preview, `done` usage tokens, etc); click (or Enter / Space, keyboard accessible) expands to the full payload. Operators scrolling a long transcript see the SHAPE of the run without a wall of text.
- **User picker dropdown** (PR #40, 2026-05-09). v0.7.3 made the operator type a `user_id` UUID into a freeform input — no way to discover who has running agents. New `GET /v1/_users` admin endpoint surfaces distinct user_ids with summary stats (`running_count`, `total_count`, `last_started_at`). The UI top bar now shows a `<select>` populated from the endpoint, refreshed every 30 s. Each option reads "user_id · N running" or "user_id · M runs" so the dropdown doubles as a quick activity view. Manual override (✎ button) preserved for picking a user with no runs yet.
- **Gemini config validation hotfix** (PR #39, 2026-05-09). v0.7.2 added the Gemini driver to `internal/providers/gemini/` and wired it into `cmd/loomcycle/main.go`'s provider resolver, but missed the validation set in `internal/config/config.go`. Operators with `provider: gemini` rows in their loomcycle.yaml saw startup fail with `unknown provider "gemini"`. Two-line fix: added gemini to `validProviderIDs`, updated the error message.

**Architecture decisions worth flagging:**

- **`store.Run.Agent` is denormalised via SQL JOIN, not stored on the runs table.** Same pattern as the existing `UserID` denorm (the comment at `store.go::RunIdentity.UserID` describes the original rationale: cheaper to trust the caller / let SQL do the join than to add a new column with its own migration story). Runs table stays unchanged; only the SELECT queries grow a LEFT JOIN onto sessions.
- **No new migrations for the Web UI features.** Everything in v0.7.3 + v0.7.4 reads from existing columns (or, for the agent name, joins sessions). The runs / sessions / events tables are unchanged from v0.5.0.
- **No SSE for live updates yet.** UI polls (`3 s` for the run list, `1.5 s` for the active-run detail, `30 s` for the user picker). v0.8 candidate: a `/v1/users/{user_id}/agents/stream` SSE endpoint pushing state-transition events. Polling is acceptable for the v0.7.x footprint; tunable inside `web/src/`.

For the v0.7.3 baseline that drove this work, see [v0.7.3](#v073--earlier).

## v0.7.3 — earlier

**Status: shipped (2026-05-09).** Adds an embedded read-only Web UI for monitoring agent runs, with bearer-in-cookie auth bridging the existing /v1 API. React + Vite + TypeScript stack; output embedded into the Go binary via go:embed. Operators visit `/ui?token=<bearer>` once; subsequent navigation authenticates via an HttpOnly session cookie.

**What's in v0.7.3 (vs v0.7.2):**

- **Embedded React SPA** (`web/` source, `internal/webui/` Go package). Two pages today: a tree view of runs at `/ui` (parent → children, filterable by status, polls every 3 s) and a per-agent detail view at `/ui/agents/{agent_id}` (event log: text / thinking / tool_call / tool_result / error / retry / done; auto-refreshes for active runs; cancel button). No new wire endpoints required — the UI consumes the existing `/v1/users/{user_id}/agents`, `/v1/agents/{agent_id}`, `/v1/sessions/{id}/transcript`, and `/v1/agents/{agent_id}/cancel` routes.
- **Bearer-in-cookie auth** (`internal/api/http/server.go::authMiddleware`). The middleware now accepts either an `Authorization: Bearer ...` header (existing contract — every adapter / curl / SDK keeps working) OR a `loomcycle_session` HttpOnly cookie set by `/ui?token=...`. Cookie is `SameSite=Strict`, `HttpOnly`, optionally `Secure` (auto-detected from `r.TLS`; operators behind TLS terminators can pass an explicit flag through `webui.Handler`).
- **`internal/webui` package**: owns the `go:embed all:dist` declaration so the Go side never reaches into `web/` directly. Exports `Handler(prefix, secureCookie)` that returns an `http.Handler` mounted by api/http at `/ui` and `/ui/`. SPA fallback: any path that doesn't resolve to an embedded asset falls through to `index.html` so the React Router handles deep links like `/ui/agents/{id}`.
- **Build pipeline**: new `make build-ui` target wraps `npm install + npm run build` and writes to `internal/webui/dist/`. CI's existing Go job stays unchanged — the committed `.gitkeep` placeholder ensures `go:embed` always has a matching file, so a fresh checkout without `npm` toolchain still compiles. A new `web-ui` CI job runs typecheck + build separately. Operators who skip the UI build see a clean 503 with `ui_not_built` code on `/ui` rather than a confusing 404.

**Architecture decisions worth flagging:**

- **No new SSE wire surface for live updates yet.** The UI polls (`3 s` for the run list, `1.5 s` for the active-run detail). v0.8 candidate: a dedicated `/v1/users/{user_id}/agents/stream` SSE endpoint that pushes state-transition events. Polling is acceptable for the v0.7.3 footprint; the consumer-side knob lives in `web/src/pages/`.
- **No source maps in the production bundle.** Inline source maps blew the embedded JS to 2 MB; separate `.map` files would still bloat the binary. UI bugs are debugged via `npm run dev` against a running loomcycle (Vite proxies `/v1` to `localhost:8787`). Production embedded payload: ~239 KB JS / ~5 KB CSS / 0.4 KB HTML (76 KB / 1.5 KB / 0.27 KB gzipped).
- **Stack: React + Vite + TypeScript.** Operator chose React over server-side-rendered HTML to keep the door open for richer extensions (tool-use hook editor, resolver matrix dashboard, CV diff viewer) without rewriting. Bundle size is small enough that the overhead vs SSR is negligible at the v0.7.3 footprint.
- **`web/dist/` is NOT committed.** The build artefact lives at `internal/webui/dist/` and is gitignored except for the `.gitkeep` placeholder. Operators / CI run `make build-ui` before `go build` (or `make build-all` which combines them). This keeps PR diffs free of bundled-asset noise.

For the v0.7.2 baseline that drove the v0.7.3 batch, see [v0.7.2](#v072--past).

## v0.7.2 — past

**Status: shipped (2026-05-09).** Adds the Google Gemini provider as the fifth backend alongside Anthropic / OpenAI / DeepSeek / Ollama. No changes to existing drivers; per-agent yaml gains `provider: gemini` as an option.

**What's in v0.7.2 (vs v0.7.1):**

- **Gemini driver** (`internal/providers/gemini/`) — new from-scratch implementation of the `Provider` interface for Google's generativelanguage.googleapis.com `/v1beta/models` API. Three wire-shape differences from the OpenAI driver kept the existing wrapper-pattern off the table:
  - The model name is in the URL path (`/v1beta/models/{model}:streamGenerateContent`), not the request body.
  - Auth is via the `x-goog-api-key` header (Vertex AI deployments override `GEMINI_BASE_URL` and supply a service-account access token).
  - Streaming requires `?alt=sse` — without it, Gemini buffers the entire response into a JSON array.
- **Tool dispatch via functionCall / functionResponse parts.** Gemini's content-part union differs from the OpenAI `tool_calls` array shape; the driver translates loomcycle's `tool_use` / `tool_result` content blocks transparently. Tool IDs are synthesised by the loop because Gemini doesn't issue them (same as Ollama).
- **Effort hint → `generationConfig.thinkingConfig.thinkingBudget`.** Gemini-2.5-flash and gemini-2.5-pro support the `thinkingConfig` knob; the driver maps `effort: low` → 0 (disable), `medium` → 2048, `high` → 8192 (clamped to `max_tokens - 1024` when the budget would equal or exceed `max_tokens`). Same vocabulary the operator already uses for Anthropic / OpenAI — no per-provider effort dialect.
- **Probe + ListModels via `GET /v1beta/models`.** Stripping the `models/` prefix the API uses internally so the resolver matches against bare aliases (`gemini-2.0-flash`) consistent with the other drivers.
- **Resolver matrix integration.** `cmd/loomcycle/main.go` registers Gemini alongside the existing four backends. Excluded when `GEMINI_API_KEY` is unset; probed at startup and on the periodic re-probe with the same 5 s deadline as the others.

**Operator-facing surface:**

- New env: `GEMINI_API_KEY`, `GEMINI_BASE_URL` (optional; defaults to public Gemini API).
- Per-agent yaml: `provider: gemini` and `model: gemini-2.5-flash` (or any model the wire `/v1beta/models` returns). Tier candidates can list `{ provider: gemini, model: gemini-... }` alongside the other backends.

**Architecture decisions worth flagging:**

- **No EventThinking from Gemini yet.** Gemini-2.5 emits `thoughtSignature` blobs (base64) rather than a text trace, so there's nothing to surface as `EventThinking`. The thinking-token *count* lands on `Usage` for cost retros (`thoughtsTokenCount` in usageMetadata). When Google opens up a text-trace surface this is the wiring point.
- **Native `cache_control` not exposed.** Gemini has implicit prompt caching on long contexts but no operator-controlled knob like Anthropic's `cache_control` breakpoint. Capability flag stays false until Gemini ships an explicit cache-control surface.

For the v0.7.1 baseline that drove this work, see [v0.7.1](#v071--earlier).

## v0.7.1 — earlier

**Status: shipped (2026-05-09).** Tag `v0.7.1` on `main`. v0.7.1 is a "consolidation" point release on top of v0.7.0: eleven PRs merged over a single intensive session that cleaned up production-discovered gaps from the jobs-search-agent integration, expanded the typed event surface (live thinking, tool-use hooks), unblocked silent-network-drop scenarios for fan-out agents, and exposed the in-process resolver matrix over HTTP so dashboards can render it. No breaking wire changes — every consumer that worked against v0.7.0 keeps working unchanged.

**What's in v0.7.1 (vs v0.7.0):**

- **DeepSeek thinking-mode roundtrip** (PR #25). DeepSeek V4 Pro / deepseek-reasoner returns `reasoning_content` alongside `content`; the API requires it to be echoed back on subsequent turns or the next request 400s. The OpenAI driver captures the reasoning trace, surfaces it on `EventDone.Reasoning`, and the request builder serialises it back when the assistant Message carries one.
- **Ollama qwen3 tool-call-as-text recovery** (PR #26 + PR #35). qwen3:14b sometimes loses tool-call envelope discipline mid-conversation. PR #26 added recovery for the JSON-shape: `{"name":"...","arguments":{...}}`, optionally inside a markdown fence, and the array form for batched calls. PR #35 added the bracketed-markdown form: `[tool_use: name]\n{args}` / `[tool_use: name {args}]` / bare `[tool_use: name]`.
- **`TestBashTimeout` race-detector reliability** (PR #27). Moved the timing-sensitive timeout test behind a `//go:build !race` tag — the race detector's goroutine-scheduling overhead starves the kill goroutine long enough that the full `sleep 5` runs to completion. Production code is fine; the race environment isn't a useful place to validate timing-sensitive scheduling.
- **Per-token text coalescing for OpenAI / DeepSeek** (PR #28). Streaming text deltas accumulate into 64-byte chunks before emitting `EventText`, with mandatory flushes on newline / before tool_calls / end-of-stream. Closes the "every word on its own line" cosmetic noise DeepSeek's tokenizer produced in line-prefix-logging consumers.
- **SSE wire-level keepalive** (PR #29). Long-lived agent streams emit `: keepalive\n\n` comment frames every 20 s by default to keep the underlying TCP/HTTP path warm. Closes the opaque `TypeError: terminated` undici reports when networks with idle-connection timeouts (Tailscale, NAT routers) drop a silent stream. Configurable via `LOOMCYCLE_SSE_KEEPALIVE_MS`; 0 disables.
- **Parallel tool dispatch** (PR #30). The agent loop dispatched a turn's tool_calls serially — for the `Agent` built-in tool that turned 3-way fan-outs into queues. New `executePendingTools` runs each tool_call in its own goroutine, bounded by `LOOMCYCLE_TOOL_PARALLELISM` (default 8). Messages handed back to the model preserve tool_call order; SSE events emit in completion order so live consumers see fast tools' results first.
- **`EventThinking` event type** (PR #32). Live streaming of the model's reasoning trace as a typed event distinct from `EventText`. All three drivers wire it up: Anthropic from `thinking_delta` content blocks, OpenAI / DeepSeek from `delta.reasoning_content`, Ollama from `message.thinking`. Consumers can render or hide the trace independently of the user-visible answer; `EventDone.Reasoning` still carries the consolidated trace for next-turn echo (DeepSeek roundtrip requirement).
- **Tool-use hooks** (PR #33). Operator-supplied middleware around tool dispatch. External apps register HTTP-webhook callbacks against `(agent, tool, phase)` selectors via `POST /v1/hooks`; loomcycle invokes them around `executeTool` so a hook can rewrite the input, short-circuit with a synthetic result, or rewrite the post-tool result. Per-`(owner, name)` idempotent registration prevents cascading on app restart. Fail-open default (telemetry hooks don't block); fail-closed available for security-shaped hooks. See `docs/TOOLS.md` for the full surface.
- **Resolver Snapshot endpoint** (PR #34). `GET /v1/_resolver` exposes the in-process availability matrix as JSON, bearer-authed. Returns 503 in the brief degraded-startup window before `SetResolver` is called, so dashboards can distinguish "matrix not available" from "matrix is empty". Wire shape uses snake_case via a thin adapter so internal `resolve.Availability` renames don't churn the public surface.
- **gofmt / chore** (PR #31). Whole-tree gofmt to clear pre-Go-1.19 doc-comment style drift that had been failing CI's gofmt step on every PR.

**Architecture decisions worth flagging:**

- **Hooks chose HTTP-webhook over Go interface for the v0.7.x scope.** External apps (jobs-search-web) need to plug their own logic in from outside the loomcycle binary. A future in-process Go hook is just a hook implementation that runs the callback in-process; the registration shape stays the same.
- **Hooks are NOT persistent across loomcycle restart.** Apps re-register on their own startup. (Owner, name) tuple identity prevents cascading on the registering app's restart. An app that's down can't process callbacks anyway, so unregistered-on-restart matches reality.
- **Parallel tool dispatch caps at 8 concurrently.** Set by `LOOMCYCLE_TOOL_PARALLELISM`. The HTTP server's `MAX_CONCURRENT_RUNS` slot still bounds the run tree, so this is an inner-loop knob that doesn't change the global ceiling. Default 8 chosen empirically — typical Anthropic / DeepSeek turns emit 2–5 tool_calls; 8 covers the common case without spawning storms on rare large fan-outs.
- **EventThinking is additive, not a replacement.** `EventDone.Reasoning` still carries the consolidated trace for the next-turn echo. Adapters that only want the final string keep working unchanged; adapters that want live progress consume both.

For the v0.7.0 baseline that drove the v0.7.1 batch, see [v0.7.0](#v070--earlier).

## v0.7.0 — earlier

**Status: shipped (2026-05-08).** Tag `v0.7.0` on `main`, merged via PRs #21 + #22 + #23. Adds the model-resolution matrix: agents declare a tier (low / middle / high) plus an optional effort hint, the runtime picks (provider, model) against an availability matrix that's live-probed at startup and re-probed every 15 minutes. Closes the v0.7+ near-term scope from v0.6.0.

**What's in v0.7.0 (vs v0.6.0):**

- **Tier-based resolution.** Agent yaml declares `tier: low | middle | high`; resolver walks library priority + tier candidates and picks the first available `(provider, model)`. Per-agent overrides for `providers:` (full priority replacement) and `models:` (full tier-candidate replacement) cover the asymmetric pinning cases — see the cv-generator / ai-detector example in `loomcycle.example.yaml`. Explicit `provider:+model:` pins from v0.6.x continue to work unchanged.
- **Live probes per provider.** `internal/providers.Provider` gains `Probe(ctx) error` and `ListModels(ctx) ([]string, error)`. Each driver implements its variant: Anthropic / OpenAI / DeepSeek hit `/v1/models`, Ollama hits `/api/tags`. Startup runs all configured probes in parallel with a 5-second deadline; results seed the matrix.
- **`Excluded` flag for unconfigured providers.** Providers without API keys (or for Ollama, no base URL) are explicitly marked `Excluded` in the matrix — distinct from "probe attempted but failed". `Snapshot()` surfaces both flags so dashboards can render "deliberately not enabled" apart from "tried and failed". Operators see startup logs like `resolve probe: deepseek excluded (DEEPSEEK_API_KEY not set)`.
- **Reactive stall feedback.** Loop calls `MarkStalled(provider, model, reason)` on driver errors that suggest the model is broken (5xx after retry, mid-stream errors). `ctx.Err()` guards prevent user-cancellations from polluting the matrix. Next periodic probe revives or confirms the stall. Stall is per-`(provider, model)` so one bad model doesn't take down a whole driver.
- **Per-driver effort translation.** Agent yaml declares `effort: low | medium | high` alongside the tier. Drivers translate where supported: Anthropic → `thinking.budget_tokens` (low → skip; medium → 2048; high → 8192; haiku always skips); OpenAI → `reasoning_effort` (pass-through verbatim); DeepSeek inherits OpenAI; Ollama is a no-op. The loop logs `effort dropped` once per Run when an agent declared effort but landed on a `SupportsEffort=false` provider.
- **Periodic re-probe.** Background goroutine on the configured cadence (default 15 min, max 1 hour via `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS`). Tied to `bgCtx` for graceful shutdown alongside the heartbeat sweeper and session-lock GC.
- **Constant-time bearer compare hardening (carry-over from v0.6.0).** `internal/auth.CompareBearer` (sha256+CTC) replaces raw `subtle.ConstantTimeCompare` on both HTTP and gRPC. Closes the length-leak side channel.

**Architecture decisions:**

- **Anthropic budget clamping.** Anthropic's API requires `thinking.budget_tokens < max_tokens` AND `≥ 1024`. When the requested budget would equal or exceed `max_tokens`, the driver clamps to `max_tokens - 1024` (leaves 1024 for the response). Below the 1024 minimum, the field is dropped entirely. Operators wanting `tier: high + effort: high` to actually get the full 8192 thinking budget should set `max_tokens: 16384` explicitly.
- **No live cutover for stall** — operators see `runs.model` reflecting the wire-resolved alias from PR 2 onwards, so cost retros remain accurate even when the resolver fell through to a different candidate mid-Run.
- **No `runs.tier` column** — the resolver's input (the requested tier) and its output (resolved provider+model) are both observable today. Adding a column for "what tier did this come from" was deferred until a consumer asks for it.

For the v0.6.x cost-routing strategy that drove this work, see [v0.6.0](#v060--earlier).

## v0.6.0 — earlier

**Status: shipped (2026-05-08).** Tag `v0.6.0` on `main`, merged via PRs #18 + #19 + the OpenAI driver `Usage.Model` fix. Provider matrix now covers four backends: Anthropic for user-sensitive paths, DeepSeek for high-volume public-data work, Ollama (local llama) for offline / cost-floor scenarios, OpenAI for general use. Per-agent provider routing in YAML lets a consumer mix and match by data sensitivity.

**What's in v0.6.0 (vs v0.5.5):**

- **DeepSeek provider** (`internal/providers/deepseek/`) — thin wrapper around the existing OpenAI driver with the DeepSeek base URL pre-baked and `ID()` returning `"deepseek"`. DeepSeek's Chat Completions endpoint is OpenAI-compatible, so the wire shape, SSE framing, retry strategy, and tool-call envelope all reuse the OpenAI plumbing unchanged. Operator opts in via `DEEPSEEK_API_KEY` env; `DEEPSEEK_BASE_URL` overrides for self-hosted OpenAI-compatible mirrors (e.g. vLLM serving a DeepSeek model). Per-agent yaml: `provider: deepseek`.
- **OpenAI driver: `Usage.Model` populated from chunk envelope.** Pre-fix, `streamEvents` extracted token counts from the streamed `usage` chunk but never captured the model field, so `runs.model` was always empty for any OpenAI-compatible endpoint (OpenAI itself, DeepSeek, vLLM, ...). Same regression class as the v0.4.0 anthropic fix; latent until the DeepSeek live test surfaced it. Now every OpenAI-compatible run records the wire-resolved alias (e.g. `deepseek-chat-v3-0324`, `gpt-4o-mini-2024-07-18`) — what the provider actually billed against, which matters for cost retros.
- **Ollama live integration tests** (`internal/providers/ollama/live_test.go`) — three tests (probe, chat, tool call) gated by `OLLAMA_TEST_BASE_URL`. v0.4.0 shipped the Ollama driver with httptest fakes only; the live coverage validates qwen3:14b on RTX 5080 (16GB VRAM) end-to-end as the offline / cost-floor backend.
- **`internal/auth.CompareBearer`** — sha256+CTC helper used by both HTTP and gRPC bearer-token middleware. Plain `subtle.ConstantTimeCompare` returns 0 immediately on length mismatch, leaking the expected token's length via timing; hashing both sides to a fixed-length digest before the constant-time compare closes that channel. Strengthens auth on both wire surfaces.

**Provider routing intent (jobs-search-agent first):**

| Sensitivity | Backend | Rationale |
|---|---|---|
| User-sensitive (CV / CL generation, profile-aware q&a) | **Anthropic** | Best quality + privacy posture for user data |
| Public data (job scoring, ATS filtering, company profiling, position relevance) | **DeepSeek** | Order-of-magnitude cheaper than Anthropic for high-volume work |
| Offline / private / cost floor | **Ollama** (local llama) | qwen3:14b on RTX-class VRAM gives DeepSeek-comparable quality at near-zero marginal cost |
| General / new agents during development | **OpenAI** | Standard choice for prototyping; pin to a specific model to avoid drift |

Architecture decision: DeepSeek is a separate `provider: deepseek` rather than `provider: openai` with a quirky base URL. Three reasons: (1) explicit yaml config documents intent, (2) per-provider `runs.model` rollups can't conflate OpenAI and DeepSeek pricing, (3) a place to absorb DeepSeek-specific quirks (reasoning_content for the reasoner model, future rate-limit header differences) without polluting the OpenAI driver.

## v0.5.5 — earlier

**Status: shipped (2026-05-08).** Tag `v0.5.5` on `main`, merged via PR #16. Wire surface coverage: gRPC alongside HTTP+SSE, async Python adapter as a first-class consumer.

**What's in v0.5.5 (vs v0.5.0):**

- **gRPC server** (opt-in via `LOOMCYCLE_GRPC_ADDR`). All seven RPCs mirror the HTTP+SSE surface 1:1 — `Run`, `Continue`, `GetAgent`, `CancelAgent`, `ListUserAgents`, `GetTranscript`, `Health`. Both wires share the same store, cancel registry, runner, and concurrency semaphore — picking gRPC vs. HTTP is a wire-format decision, not a feature decision. A cancel issued via gRPC reaches a run started via HTTP and vice versa.
- **`internal/runner/`** wire-agnostic seam. The HTTP server satisfies `runner.Runner`; the gRPC server delegates to the same instance. Compile-time guard `var _ runner.Runner = (*Server)(nil)` keeps the interface conformance honest.
- **Python adapter** (`adapters/python/`, `pip install loomcycle`) — async `LoomcycleClient` over `grpc.aio` covering all seven RPCs. Frozen-dataclass events (`AgentEvent`, `ToolUse`, `Usage`, `Retry`); typed exceptions over `grpc.StatusCode` codes; PEP-561 `py.typed` marker. Promotes the Python adapter from "deferred" to a shipped consumer.
- **Synthetic registration frames.** `Run`/`Continue` server-stream emit a wire-stable `session` + `agent` frame pair before any provider event so adapters can capture `(agent_id, run_id, session_id, parent_agent_id)` without re-decoding the loop's transcript. The Python adapter swallows these into a `RunHandle`; the HTTP+SSE side-channel exposes the same data via existing event types.
- **Operator guide:** [GRPC.md](GRPC.md) covers enablement, wire-shape parity with HTTP+SSE, synthetic-frame contract, error code mapping, TLS / coexistence recipes, Python adapter quick-start.

## v0.5.0 — earlier

**Status: shipped (2026-05-08).** Tag `v0.5.0` on `main`, merged via PR #15. The production-deployment unlock: Postgres `Store` adapter alongside SQLite (which stays first-class for compact installs), heartbeat sweeper + session-lock map GC, operator-facing CLI surface.

**What's in v0.5.0 (vs v0.4.0):**

- **Postgres `Store` adapter** (`internal/store/postgres/`) — full implementation over `pgx/v5` + `pgxpool`, embedded migrations via `golang-migrate/migrate v4`. Operator opts in via `storage.backend: postgres` (yaml) or `LOOMCYCLE_STORAGE_BACKEND=postgres` (env). Postgres ≥ 14 required.
- **SQLite stays first-class.** Default backend; both adapters validated against a shared behavioural contract suite (17 sub-tests) so they cannot drift silently.
- **Heartbeat sweeper** — periodic background goroutine marks runs whose process crashed mid-loop as `failed`. Default-on; `LOOMCYCLE_HEARTBEAT_*` env knobs control cadence + cutoff.
- **Session-lock map GC** — refcounted + idle-pruned; closes the v0.3.2 leak where the per-session continuation mutex map grew monotonically.
- **CLI subcommands** (`loomcycle <verb>`) — `validate`, `agents list`, `health`, `migrate up|down|status`, `migrate sqlite-to-postgres`. The migration tool copies an existing SQLite DB into Postgres with row-count + transcript-digest verification, idempotent on re-run.
- **Operator guide:** [POSTGRES.md](POSTGRES.md) covers configuration, the auto-migrate vs explicit-migrate policy split, the sqlite→postgres runbook, and reference benchmark numbers (100 concurrent agents: SQLite p99=31ms, Postgres p99=60ms — both well under the 1-second acceptance threshold).
- **`make pg-up` / `pg-down`** — Docker-based Postgres test fixture for local dev.

**Architecture decisions (vs original v0.5.0 plan):**

- **Cross-replica advisory locks deferred to v1.0.** Driver was multi-replica HA; for the only deployment shape today (single replica), the in-memory cancel registry works for both backends.
- **No live cutover for sqlite→postgres** — operator stops loomcycle, runs the copy, restarts. Live cutover is v1.0.

## v0.4.0 — earlier

**Status: shipped.** Tag `v0.4.0` on `main`. The runtime's MCP integration story is now production-validated against jobs-search-agent as the first real consumer.

**What's in v0.4.0:**

- Three providers — Anthropic Messages (with native `cache_control`), OpenAI Chat Completions, Ollama `/api/chat` (tool-tuned models only).
- Nine built-in tools — `Read`, `Write`, `Edit`, `HTTP`, `WebFetch`, `WebSearch`, `Bash`, `Agent` (sub-agent spawning), `Skill` (Approach A static bundling).
- MCP integration — pooled stdio children with auto-respawn, **Streamable HTTP transport** (Accept: both JSON + SSE per spec; SSE response decoder; per-call dial), per-server allowlists.
- **MCP startup retry** — exponential backoff handshake (500ms → 16s capped, 30s budget) so a peer compiling its `/api/mcp` route on first request doesn't get marked "skipped" indefinitely. Resolves the chicken-and-egg start-order race that bites every dev environment.
- Sub-agents via the `Agent` built-in — depth-capped (16), parent host policy + identity inherited via ctx.
- Agent tracking + cancel API — `agent_id` per run, cascade-cancel via `parent_agent_id`, list runs per user.
- Per-agent `max_tokens` config (output budget; covers the case where bundled skills + tool narration eat into a fixed cap).
- Anthropic driver: model alias plumbed from `message_start` into final `Usage.Model` so callers can price runs against the resolved alias.
- Sub-agent caller-host policy inheritance — children inherit the parent's per-call `allowed_hosts` instead of falling back to the operator's static list.
- SQLite store — sessions, runs, events; partial indexes for v0.4 sub-agent columns; WAL mode.
- Concurrency — global semaphore + bounded FIFO queue; backpressure → HTTP 429.
- TypeScript adapter (`@loomcycle/client`) shipped on npm.
- **End-to-end MCP-server validation against jobs-search-agent.** Two agents (`ats-filter`, `qa-agent`) now fetch context — and `qa-agent` also persists results — through typed `mcp__jobs__*` tools served by jobs-search-agent's own `/api/mcp` Streamable-HTTP server. This validates the runtime's MCP HTTP transport, host-policy inheritance, sub-agent retry, SSE response decoding, and Streamable-HTTP `Accept` handling against a real consumer. Per-agent migration in the consumer continues incrementally; the loomcycle surface is stable.

**Architecture pivot worth flagging.** v0.4.0 was originally planned as "migrate jobs-search-agent to the LocalAPI gateway" — feed an OpenAPI spec to loomcycle, register typed tools per operation. During the implementation, the user pulled back: the LocalAPI shape couples loomcycle's deploy config to one consumer's surface (loomcycle has to know about jobs-search-agent's routes). The cleaner architecture: jobs-search-agent runs its own MCP server; loomcycle stays domain-agnostic and consumes any MCP server via its existing HTTP MCP transport. v0.4.0 ships that pivot. LocalAPI remains in the codebase as a future-consumer convenience for OpenAPI-without-MCP-server cases, but it's not the integration vehicle.

**Scaffolded but not the v0.4 vehicle:**

- **LocalAPI MCP gateway** — code is in `internal/tools/localapi/`, parser + dispatcher wiring + unit tests all landed, registered into the dispatcher at boot when `cfg.LocalAPI.SpecPath` is non-empty. Useful for future consumers that have an OpenAPI spec and don't want to stand up an MCP server.

For usage: see [README](../README.md). For the architecture: see [ARCHITECTURE.md](ARCHITECTURE.md). For tool policy: see [TOOLS.md](TOOLS.md).

## v0.8.x — next: framework primitives

Sequenced 2026-05-09; renumbered 2026-05-10 after v0.8.1 absorbed three operational-hardening PRs (#47/#48/#49); renumbered again 2026-05-11 after v0.8.2 absorbed the `user_tier` resolver-overlay + runtime-fallback work (PRs #52/#53); renumbered once more 2026-05-11 to insert v0.8.5 Self-Evolution Substrate (AgentDef + versioning + Evaluation) ahead of Context (introspection) and LoomCycle MCP — the capstone must expose those tools, so they have to ship first; v0.8.3 took the ollama-split hot-fix and v0.8.4 shipped the Channel tool, so the remaining v0.8.x roadmap below begins at v0.8.5; **v0.8.8 Question tool added 2026-05-11 as the human-in-the-loop primitive that completes the substrate symmetry — Memory + Channel + AgentDef/Eval + Context + LoomCycle MCP + Question covers every direction of agent interaction (state, IPC, self-mutation, introspection, control plane, human bridge).** **v0.8.9 Pause / Resume / Snapshot added 2026-05-12 as the v0.8.x → v0.9.x bridge — runtime-wide quiesce + cross-version-portable JSON snapshot, the precondition for v0.9.x multi-replica HA and the missing primitive for long-running experiments surviving provider rate-limits and infrastructure maintenance. The v0.8.6 Context tool gains a ninth op (`history`) at the same time, paired with the snapshot's optional interaction-history bundling.** Each point release in the framework-primitive sequence ships one focused capability — the v1.0 capstone (LoomCycle MCP) needs them in this order because the MCP server's surface is built FROM these primitives. Detailed design (API schemas, storage shapes, retention semantics) lives in feature-branch RFCs at implementation time; the outlines below capture the shape but not the wire.

**v0.8.0 Memory tool shipped 2026-05-09**; **v0.8.1 operational hardening shipped 2026-05-10**; **v0.8.2 user_tier shipped 2026-05-11**; **v0.8.3 ollama split shipped 2026-05-11**; **v0.8.4 Channel tool shipped 2026-05-11** — see the sections above for full release notes.

### v0.8.5 — Self-Evolution Substrate (AgentDef + versioning + Evaluation)

Three substrate primitives shipped together; closes the gaps that block the planned self-evolving agentic research program. Sequenced before Context (v0.8.6) and LoomCycle MCP (v0.8.7) because the MCP capstone must expose both new tools.

**Primitive 1 — `AgentDef` built-in tool.** Agents can `create` / `fork` / `retire` / `promote` / `get` / `list` agent definitions at runtime. Six-op surface, mirrors the Memory tool's discriminated-op shape. Operator-blessed static `<name>.md` files remain the immutable root; DB-held versions live in the derived layer. Per-agent yaml `agent_def_scopes` gate (`self` / `descendants` / `named:[...]` / `any`), default-deny. `AllowedTools` widening is REFUSED across forks — operator-blessed root is the permanent capability ceiling.

**Primitive 2 — Versioned definitions + lineage.** Two new tables: append-only `agent_defs` (UUID `def_id`, monotonic `version` per `name`, `parent_def_id` for lineage, `bootstrapped_from_static` flag for static-derived roots) + `agent_def_active` pointer table for "which version a name resolves to." Additive `runs.agent_def_id` column for cost / experiment retros. Resolver becomes DB-first with static fallback: `defID` pin → `agent_def_active[name]` → `cfg.Agents[name]`. Sub-agent spawning re-resolves by name + uses active pointer; pinning via optional `def_id` on the `Agent` tool input.

**Primitive 3 — `Evaluation` built-in tool.** Five-op surface (`submit` / `get` / `list_for_run` / `list_for_def` / `aggregate`) over a new `evaluations` table. Score model: required scalar (RL lingua franca) + optional `dimensions` map (multi-axis fitness) + optional `judgement` JSON + optional `rationale` text. Emitter role derived server-side from ctx (`self` / `sibling` / `parent` / `external` / `unrelated`) so the model can't lie about who scored what. Per-agent yaml `evaluation_scopes` gate (multi-select). Loomcycle does NOT auto-promote based on score — selection is policy, lives in user-agent orchestrators.

**Three motivating experiments** the substrate enables: (A) existing hockeybot pipeline — zero-regression bar; (B) software-dev team where orchestrator forks specialized coder variants per task and a sibling reviewer scores them; (C) ecosystem evolution with random agents, grow/survive/eat/multiply/die rationales, lineage-based analysis.

Detailed design in `doc-internal/rfcs/self-evolution-substrate.md` (gitignored). Build sequence: 7 PRs over ~3–4 weeks. PRs 1–5 are the experiment-enabling substrate (all three experiments run after PR 5 with no UI); PRs 6–7 are the admin API + Web UI agent-defs page — load-bearing for the v1.0 "Agentic OS" launch narrative.

### v0.8.6 — Context tool (runtime introspection)

Read-only tool that lets agents introspect their own runtime — what tools they have, what their identity / lineage looks like, what evaluations exist for their definition, what channels they can reach. Discriminated `op` field (same shape as Memory) lets agents narrow output instead of paying for a kitchen-sink dump on every call.

**Nine ops:**

| Op | Returns | Params |
|---|---|---|
| `self` | Identity bundle: `agent_id`, `agent_name`, `run_id`, `parent_agent_id`, `user_id`, `user_tier`, `agent_def_id` + `version` + `parent_def_id`, `iteration`, `provider`, `model`, loaded `skills`, Memory key-counts per scope | — |
| `tools` | Post-filter tool catalog — builtins + MCP (`mcp__server__name`). Each entry: `name`, `description`, `side_effect_class`, runtime narrowing | — |
| `doc` | Detailed tool documentation: input schema, op surface, examples, operator notes | `name?` |
| `agents` | Visible agents — `name`, `description`, `tier`, `active_def_id` + `version`, spawn/mutate eligibility flags. **No `system_prompt` body** — that's behind `AgentDef.get` so the `agent_def_scopes` gate applies | `prefix?` |
| `permissions` | Gates that apply to caller: `allowed_tools`, `allowed_hosts`, `memory_scopes`, `agent_def_scopes`, `evaluation_scopes`, `history_scope`, accessible MCP servers, active hooks | — |
| `channels` | Accessible channels: name, description, publish/subscribe rights, recent activity counter | `prefix?` |
| `lineage` | AgentDef lineage tree: ancestors + descendants + retire/promote markers (from v0.8.5 substrate) | `def_id?` (defaults to caller's); `depth?` |
| `evaluations` | Evaluation aggregate: count, mean/median/min/max/latest, per-emitter-role breakdown, dimensions stats (from v0.8.5 substrate) | `def_id?`; `include_lineage?` |
| `history` | Conversation events for one or more runs: text / thinking / tool_use / tool_result / usage / error / retry / done. Lets self-evolving agents reflect on past conversations and reproduced experiments (v0.8.9 snapshots) replay history into restored agents. **Gated by per-agent `history_scope` yaml field** (`self` / `siblings` / `descendants` / `named: [...]` / `any`, default-deny). Paginated for large histories | `run_id?` / `agent_id?` / `since?` / `until?` / `event_types[]?` / `limit?` / `cursor?` |

**Default-add behaviour.** `Context` is auto-added to every agent's `allowed_tools` at config load — missing introspection is a footgun for self-evolving agents. Operators who want airgapped agents opt out via per-agent yaml `disable_context: true`.

**Privacy boundary.** `Context.agents` returns metadata only (names, descriptions, tier). The full `system_prompt` body lives behind `AgentDef.get(def_id)`, which is gated by `agent_def_scopes`. Two layers: cheap discovery via Context, capability-gated content access via AgentDef.

Single read-only tool — no mutations, no side effects beyond reads. Useful for the Comfort Agents pattern — agents that build their own task plans benefit from being able to inspect their environment before deciding what to do.

What's not yet decided: response-size caps per op (default `LOOMCYCLE_CONTEXT_MAX_RESPONSE_BYTES`), schema for the `side_effect_class` taxonomy (pure / network / filesystem / privileged / state), whether `agents` shows agents the caller cannot spawn (current lean: yes, with an eligibility flag — discovery should be transparent even when access isn't).

### v0.8.7 — LoomCycle MCP (the v0.8.x capstone)

Loomcycle exposes itself as an **MCP server**. External orchestrators (Claude Code, agentic harnesses, other loomcycle instances) connect to it as an MCP client and:

- Configure agents and spawn runs (alternate front-end to `/v1/runs`).
- Send messages on Channels (built in the v0.8.3–0.8.4 Channel work).
- Read/write Memory entries (built in v0.8.0).
- **Create / fork / promote `AgentDef` versions (built in v0.8.5).**
- **Submit / aggregate Evaluations (built in v0.8.5).**
- Call Context for runtime introspection (built in v0.8.6).
- **Surface `Question.ask` so external orchestrators can route human-in-the-loop questions through their own UI** (paired with v0.8.8 Question tool — the loomcycle Web UI is the default surface, the MCP exposure is the orchestrator-side override).
- Subscribe to run-event streams (alternate to SSE).

This is the "MCP-configurable" axis: instead of writing YAML and POSTing JSON, an external tool drives loomcycle through standard MCP. Surface area maps roughly 1:1 to the existing `/v1/*` endpoints plus the v0.8.0–0.8.6 primitives, with auth via the operator's bearer token translated into MCP's auth scheme.

What's not yet decided: stdio vs HTTP transport (probably both — stdio for desktop-app integrations, HTTP for service-to-service), method naming (resources vs tools), whether MCP clients can register new agents at runtime or only spawn from operator-defined ones, handling of long-lived run streams across MCP's request/response shape.

### v0.8.8 — Question tool (human-in-the-loop primitive)

Closes the one direction the v0.8.0–v0.8.7 arc leaves uncovered: **agent ↔ human, mid-run**. The substrate primitives so far cover machine interaction — state (Memory), IPC (Channel), self-mutation (AgentDef), selection (Evaluation), introspection (Context), control plane (LoomCycle MCP). `Question` is the human bridge.

Agents call `Question.ask` to surface a clarifying question, request approval before a risky action, or present a multiple-choice decision to a human in the loop. The human's answer flows back into the agent's loop as the tool result.

**Tool surface (sketch):** three ops on the discriminated-`op` shape that matches Memory / Channel / Context.

- `ask(question, options?, context?, timeout_ms?, priority?)` — surface a question; agent loop pauses until answered, timed out, or cancelled.
- `notify(message, priority?)` — fire-and-forget (no answer expected).
- `cancel(question_id)` — cancel a pending question (e.g. agent figured it out itself before the human got there).

The result of `ask` carries `{question_id, answered, answer?, answered_by_user_id?, answered_at?, timed_out, cancelled}`. The model never supplies `answered_by_user_id` — server-resolved from the bearer / cookie context of whoever submits the answer.

**Three delivery surfaces, one tool interface:**

| Path | Where the human sees it | Owner |
|---|---|---|
| **(1) Built-in with Web UI default** | `/ui/questions` modal/sidebar; operator (matching the run's `user_id`) answers via the existing bearer-cookie session | loomcycle |
| **(2) MCP-implemented (consumer-side)** | Consumer runs its own MCP server exposing `mcp__<name>__ask`; loomcycle calls it like any other MCP tool (jobs-search-agent's pattern) | Consumer |
| **(3) LoomCycle MCP exposure** | v0.8.7 LoomCycle MCP surfaces `Question.ask` so external orchestrators (Claude Code, custom dashboards) become the delivery surface | loomcycle + orchestrator |

Operator picks via `question.backend: webui | mcp_server:<name> | cli` in yaml. The Web UI is the default for production; `cli` lets local dev read answers from stdin.

**Trust model:** per-agent `question_enabled: true` (default-deny, parallel to `memory_scopes` / `channels`). The run's `user_id` is the authoritative answerer — only that user (or operator admins via bearer) can submit. Operator env caps `LOOMCYCLE_QUESTION_DEFAULT_TIMEOUT_MS`, `LOOMCYCLE_QUESTION_MAX_TIMEOUT_MS`, `LOOMCYCLE_QUESTION_MAX_PENDING_PER_USER` bound the surface against runaway question floods.

**Storage:** new `questions` table (additive migration, same pattern as `memory` / `channel_messages` / `agent_defs`). Wire: `POST /v1/runs/{run_id}/questions/{question_id}/answer` for the operator-side submit; new SSE event types `EventQuestion` (emitted on `ask`) and `EventQuestionAnswered` (emitted on resolve).

**Run-lifecycle integration:** when `ask` is called, the agent loop enters a "waiting on human" state. The existing v0.5.0 heartbeat keeps the run alive; the v0.8.4 Channel long-poll wake pattern is reused for sub-millisecond wake on answer.

**What's not yet decided:** blocking vs polling tool semantics (lean: blocking, with heartbeat keeping the run alive), the relationship to Channel (could be implemented as a thin layer; lean: separate tool because request/response semantic is one-to-one, not many-to-many), the notification side-channel (how does the human learn to look at the UI — Slack bot subscribing to SSE? email digest? — lean: out of scope, operator integrates against the pending-queue endpoint).

Detailed design at pickup time will lift the local addendum (in this session's plan file) into `doc-internal/rfcs/question-tool.md`.

### v0.8.9 — Pause / Resume / Snapshot (the v0.8.x → v0.9.x bridge)

Runtime-wide quiesce + restore primitive. Closes the gap that today forces every long-running experiment to either run-to-completion-or-die or restart from scratch when the provider rate-limits, the infrastructure needs maintenance, or the operator wants to ship the experiment somewhere else.

**Three motivating shapes the substrate enables:**

| Pain shape today | With v0.8.9 |
|---|---|
| Provider down or rate-limited (Ollama subscription paused, Gemini quota exhausted, Anthropic 5xx) → every active run dies after the v0.8.2 fallback budget exhausts | Operator pauses → waits for capacity → resumes from where each run left off |
| Pre-backup quiesce (DB hot-backup captures partial transcripts mid-iteration) | Pause → snapshot → backup → resume; consistent across runtime + DB |
| Migrate to a new VM / loomcycle version / ship a research artefact | Pause → snapshot → JSON export → restore on the new instance |

**Also: pause/resume is the precondition for v0.9.x multi-replica HA.** Multi-replica deployments need to drain a node before maintenance; runtime-wide pause provides exactly that. Sequencing pause before HA avoids retrofitting drain semantics into an already-running HA system.

**Wire surface:**

- `POST /v1/_pause?timeout_ms=30000` — runtime-wide pause. LLM streams cancel immediately; MCP tools + non-idempotent built-ins (`Bash`, `Write`, `Edit`, `HTTP`-mutate, `Memory.set/incr/delete`, `Channel.publish/subscribe/ack`, `AgentDef.create/fork/promote/retire`, `Evaluation.submit`) wait up to `timeout_ms` for completion then force-cancel. Idempotent built-ins (`Read`, `WebFetch`, `WebSearch`, `Memory.get/list`, `Channel.peek`, `AgentDef.get/list`, `Context.*`, `Evaluation.get/aggregate`) cancel immediately. New `/v1/runs` requests get 503 during pause.
- `POST /v1/_resume` — resume agent loops from each paused run's last completed iteration boundary. Re-issues the last LLM request with full transcript context; tool calls that were force-cancelled re-issue (non-idempotent tools are operator-side concerns — container isolation recommended for the agentic posture).
- `POST /v1/_snapshot` — capture running-state into a new `snapshots` table. Captures: agent_defs (full lineage), agent_def_active, memory entries, channel state (messages + cursors + config), evaluations, paused runs (with transcripts). Optional `include_history=true` + `since_ts=<RFC3339>` adds interaction history events.
- `POST /v1/_snapshots/{id}/export` — write a snapshot's JSON content to disk; portable across loomcycle versions.
- `POST /v1/_restore` — restore from a `snapshot_id` (same instance) or `json` body (cross-instance / cross-version). Per-section semver versioning lets new loomcycle versions migrate older sections without breaking the rest.
- `GET /v1/_state` — runtime state (`running` / `pausing` / `paused` / `resuming` / `restoring`) for dashboards.

CLI mirrors: `loomcycle pause` / `resume` / `snapshot` / `restore` / `state`.

**Scope decisions (locked in the v0.8.9 RFC):**

- **Runtime-wide pause**, not per-tenant. VM is the safety boundary; per-tenant pause defers to v0.9.x per-tenant fairness.
- **Snapshot scope is running-state only.** External DB backups handle archival history. Snapshot stays small (paused runs + Memory + Channel + agent_defs + evaluations).
- **JSON format is per-section semver.** Each top-level section (`agent_defs`, `memory`, `channels`, ...) carries its own `version`; reader applies per-section migrations. Suits active development where sub-schemas change at different cadences.
- **Interaction history is opt-in via flag** (`--include-history --since <ts>`). Default snapshot stays lightweight; the heavyweight reproducibility snapshot is an explicit operator action.
- **Experiment identity = timestamp** (operator supplies `since_ts`). First-class `experiments` table deferred; promote if research needs it.
- **Pause and snapshot are decoupled operations** that compose as the operator chooses. Pause alone for rate-limit-wait; pause + snapshot + export for migration; snapshot during running for backup-time-of-day capture.

**Pairs with the v0.8.6 `Context.history` op:** restored experiments contain interaction history; agents in the restored instance read it via `Context.history(since=<experiment_start_ts>)` to reflect on past conversations. Self-evolving experiments survive cross-version migration with their memory intact.

**What's not yet decided:** snapshot encryption-at-rest (operator's disk-encryption policy applies — same as transcripts), automatic snapshot scheduling (lean: out of scope; operator integrates with their own scheduler), partial restore granularity (v0.8.9 is all-or-nothing per section; selective is a v0.9.x candidate).

Detailed design in `doc-internal/rfcs/pause-resume-snapshot.md` (gitignored).

## v0.9.x — high-load runtime sweep

Cross-cutting capacity items. Not a single feature; collectively they take the runtime from "single-tenant comfortable on a 4–8 GiB VPS" to "10k concurrent agents per replica." Sequenced into v0.9.x as a series of small focused PRs once the v0.8.x framework primitives are in.

- **Per-tenant fairness** in the concurrency layer. Currently every caller competes for one global semaphore — a noisy tenant monopolises the pool. Token bucket per `user_id` (or per `tenant_id` once that lands), with a small unfair share for global priorities.
- **In-memory run-status cache.** Today every `GET /v1/agents/{agent_id}` hits SQLite/Postgres. At 10k concurrent runs this is a hot path. LRU keyed on `agent_id` with sub-second TTL.
- **OpenTelemetry traces + Prometheus metrics endpoint.** Currently logs only. Per-run trace, per-tool-call span (the v0.7.1 hook seam is the wiring point), request rate / queue depth / semaphore-occupancy / provider-RTT histograms.
- **Heartbeat sweeper.** `last_heartbeat_at` is updated by the loop on every iteration but nothing reads it. A sweeper detects crashed runs (no heartbeat for > N minutes) and marks them failed so they don't stay `running` forever. (Schema-side already in place since v0.5.0.)
- **Session-lock map GC.** The HTTP server's `sync.Map` of session locks never garbage-collects entries (~32 B per session). Periodic sweep + bounded total entries.
- **Multi-replica HA.** Postgres for transcripts (already shipped in v0.5.0); Redis for in-flight cancel registry replication. Out-of-process cancel works across replicas via Redis pub-sub.

## v1.0 — planned

The v1.0 ambition closes the loop: **10k concurrent agents per replica running on operator-friendly distribution paths**, with the v0.8.x framework primitives + v0.9.x capacity work polished into a release candidate. The v1.0 cut adds operator-experience work that wasn't worth doing earlier:

### Distribution channels

Make `loomcycle` install with one command on every operator's preferred toolchain:

- **Homebrew tap** (`brew install loomcycle/loomcycle/loomcycle`) — macOS / Linuxbrew. Tap repo with a formula generated from the GitHub release artefacts.
- **Docker images** (`docker pull ghcr.io/denn-gubsky/loomcycle:v1.0`) — multi-arch (amd64 + arm64). Distroless base, embedded UI, ~30 MB compressed. CI publishes on every release tag.
- **Kubernetes Helm chart** — values.yaml covering the env-var surface, ConfigMap for the YAML config, optional Postgres + Redis dependencies via the chart's values. Documented for both single-replica (default) and multi-replica HA modes.
- **`go install`** path stays the canonical install for Go-shop operators (`go install github.com/denn-gubsky/loomcycle/cmd/loomcycle@latest`); the Homebrew + Docker paths are convenience layers on top.

### Integration tools

First-party integrations the runtime makes more valuable:

- **Claude Desktop / Claude Code MCP integration** — pre-built `.mcp.json` recipe + a one-page operator guide for adding loomcycle to Claude Code's MCP server list (uses the v0.8.7 LoomCycle MCP surface).
- **OpenTelemetry exporter recipes** — example Helm values + `loomcycle.yaml` snippets for the three common backends (Tempo, Honeycomb, Datadog). The OTEL spans themselves ship in v0.9.x; v1.0 ships the operator-side wiring guide.
- **Prometheus / Grafana dashboard** — JSON dashboard committed to the repo, importable in one click. Key panels: throughput, error rate by provider, p99 tool dispatch latency, per-tenant share of the semaphore.
- **CLI scaffolding** — `loomcycle init` generates a minimal working `loomcycle.yaml` + `.env.local` for a fresh deploy. `loomcycle agent add <name>` scaffolds an agent yaml block. Lower the time-to-first-run for new operators from "read the docs" to "run two commands."

### Polish + operator-experience

- **Settings UI** in the Web SPA — bearer-token rotation flow, env-var inspector, hook list view (read-only, the operator's own registrations from `POST /v1/hooks`), Resolver matrix dashboard. Replaces the current `?token=…` URL-paste with a proper login form when the operator hits `/ui` without a session cookie.
- **YAML schema validation** — `loomcycle validate` already exists; v1.0 adds per-field error messages with line numbers + suggested fixes (today most errors are a single-line "unknown agent X" without further detail).
- **Long-form architecture docs** — the existing `docs/ARCHITECTURE.md` covers the runtime; v1.0 adds an operator-flow walkthrough (deploy → configure → wire a consumer → monitor) and a developer-flow walkthrough (clone → make → write a hook → publish).

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

### Web monitoring frontend (shipped — was v1.0, now v0.7.3+)

The read-only operator-facing monitoring frontend that was originally scoped here landed early as v0.7.3 (initial ship) and v0.7.4 (agent names, user picker, content panels). v1.0 builds on it with the **Settings UI** + **bearer-rotation flow** + **Resolver matrix dashboard** + **read-only hook list view** items called out under "Polish + operator-experience" above. The original outline is preserved here for traceability.

## Decision principles

These hold across v1.0 work; deviation requires a written justification:

- **Config-driven posture, no profile flag.** Operators compose their security stance from individual env + YAML keys. We do not ship a "sandbox mode" abstraction.
- **One binary stays one binary.** No `loomcycle-server` vs `loomcycle-agent`. Every feature lands in `cmd/loomcycle`. Build artefacts stay singular.
- **MCP-orchestrable.** Whatever surface we expose for agents (Memory, Channel, Context), we also expose to external MCP clients. Agents and orchestrators play on the same plane.
- **Storage is pluggable.** SQLite for dev/single-tenant; Postgres for multi-replica. Anything new (Memory, Channel) goes through the `Store` interface, not direct SQL.
- **No vendor SDKs in the loop.** Every provider driver is pure HTTP. No bundled binaries; no subprocess auth inheritance.
- **Default-deny stays default-deny.** New tools start invisible to existing agents until they opt in.

## Contribution policy

> **External contributions are closed until v1.x ships.** PRs against this repository during v0.8 / v0.9 / v1.0 development will be acknowledged and closed (not merged) without prejudice — see [`CONTRIBUTING.md`](../CONTRIBUTING.md) for the full policy, the rationale, and what's still welcome (bug reports, security disclosures, downstream consumers, forks).

The chain below applies to **internal contributors** (the maintainer + Claude Code working with the maintainer's confirmation). It captures the discipline for the v0.8 / v0.9 / v1.0 work itself.

Pick an item, write an RFC (one markdown file under `doc-internal/rfcs/<feature>.md`), open a feature branch (`feature-<name>`), follow the chain documented in `CLAUDE.md` (architect → plan → code → tests → review → merge). The RFC is the design step — implementation follows once the RFC is reviewed.

For non-trivial items (Memory tool, Channel tool, Context tool, LoomCycle MCP), the RFC should cover:

1. The user-visible surface (API shape, semantics, error cases).
2. The storage / wire shape (schema, message formats).
3. Trust model — who can call this, what's the threat case.
4. Migration plan — what existing code path changes, what stays compatible.
5. Verification — how an operator confirms the feature works end-to-end.

Small features (a new built-in tool, a new provider driver, a fix) skip the RFC and go straight to a feature branch.
