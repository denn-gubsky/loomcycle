# LoomCycle release history

Per-version release notes from v0.4.0 onward. The current and immediately previous releases are also summarised in the main [`README.md`](README.md); older releases live here.

For the **public roadmap** (planned v0.8.13 through v1.0 work — LoomCycle MCP, Question tool, Pause / Resume / Snapshot, distribution, operator postures), see [`docs/PLAN.md`](docs/PLAN.md).

For pre-v0.4 history (single-tool runtime, library milestone, security patch), see the same `docs/PLAN.md` under the per-version sections.

---

## What's in v0.8.12

| Surface             | Status |
|---------------------|--------|
| **Cross-provider `reasoning_content` strip on fallback** | ✅ When `tryProviderFallback` (`internal/loop/loop.go`) successfully switches providers mid-conversation, walks the in-flight `messages` slice and zeroes `Message.Reasoning` on every assistant turn. The new provider gets a clean history. Fixes the 2026-05-13 production bug where a `gemini-2.5-flash → deepseek-v4-flash` fallback 400'd with `"The reasoning_content in the thinking mode must be passed back to the API."` (PR #91) |
| **New typed event `EventReasoningInvalidated`** | ✅ Emitted when the strip pass cleared one or more assistant turns. Mirrors the v0.8.2 `EventCacheInvalidated` precedent. Wire-stable; consumed in the same way as other typed events. `Text` field carries: `"cleared reasoning_content from N assistant turn(s) on switch from <old> to <new>; cross-provider echo would 400"`. Cost retros should treat downstream iterations as reasoning-cold on the new provider. |
| **Safe across all current providers** | ✅ Anthropic uses typed content blocks for `extended_thinking` (not the Reasoning string field) → immune. Gemini's driver doesn't write Reasoning today → strip is a no-op unless populated via PriorMessages from a continuation. OpenAI o-series tolerates missing `reasoning_content` (treats as no prior thinking). DeepSeek + OpenAI o-series within their own family continue to round-trip correctly because the strip only fires on cross-family fallback. Tool calls in the same turn unaffected: strip only touches the `Reasoning` string field, not `Content` (tool_use blocks + tool_use_id stay intact). |
| **3 regression tests** | ✅ `TestFallback_ReasoningStrippedOnProviderSwitch` (headline regression; verified to fail on pre-fix code with the exact production failure mode), `TestFallback_NoReasoningStrip_NothingToStrip` (guards against spurious event emission when no Reasoning was set), `TestFallback_PartialStreamReasoning_NeverReachesMessages` (pins the drain-and-continue invariant for in-stream errors). New `recordingProvider` test wrapper captures the `providers.Request` the new provider receives so assertions can verify the strip happened on the wire. |
| **No env-var changes** | ✅ Existing fallback behavior preserved on same-family round-trips. No new config required. |
| **Adapter-side note** | ⚠️ TS adapter (`@loomcycle/client`) logs `[loomcycle: unknown event "reasoning_invalidated"]` until a handler is added. Cosmetic — doesn't affect run outcomes. Separate adapter PR. |

## What's in v0.8.11

| Surface             | Status |
|---------------------|--------|
| **`internal/metrics/` package** | ✅ New process-resource sampler. Periodic ticker (default 5s) reads `runtime.ReadMemStats` for Go heap + goroutine count, `/proc/self/status` for VmRSS, `/proc/self/stat` for utime+stime delta CPU%, and optionally `/proc/stat` + `/proc/meminfo` for system-wide CPU/mem. **Idle-gated on `concurrency.Semaphore.Stats().active > 0`** — when no agent runs are in-flight, no DB write, no `/proc` read. Sleep cost is one in-process atomic load per tick. |
| **`/v1/_metrics/*` HTTP API (3 endpoints)** | ✅ All bearer-authed, return 503 with `enable_hint` when sampler not configured: (1) `GET /v1/_metrics/samples?since=&until=&limit=&cursor=` — windowed raw samples with cursor pagination; (2) `GET /v1/_metrics/runs/{run_id}` — peak/mean RSS + max CPU% computed via SQL JOIN on `[started_at, COALESCE(completed_at, now)]`; (3) `GET /v1/_metrics/summary?period=1h\|24h\|7d` — aggregated buckets (mean/max RSS, p95 CPU%, max active_runs per bucket; in-Go aggregation acceptable at v0.8.x scale, ≤2016 rows for 7d/5min). |
| **Build-tag-split `/proc` readers** | ✅ `proc_linux.go` (`//go:build linux`) reads `/proc/self/status` VmRSS, `/proc/self/stat` utime+stime delta (USER_HZ=100, hard-coded), optionally `/proc/stat` + `/proc/meminfo`. `proc_other.go` (`//go:build !linux`) returns zero values + `ProcMetricsAvailable=false`. macOS/Windows dev workstations still record platform-independent fields (active_runs, goroutine count, Go heap) — RSS/CPU columns land as 0. Hardened containers (gVisor, kata) get soft-failure handling: log once, continue with zero fields. |
| **`process_samples` table** | ✅ Time-series, 12 columns. SQLite `CREATE TABLE IF NOT EXISTS` in `migrate.stmts`; index `process_samples_by_sampled_at` in `addIndexes` (defensive habit per the v0.8.6 lesson — future ALTER TABLE column adds can't break index creation order). Postgres migration `0009_process_samples.up.sql` with `TIMESTAMPTZ` + `BIGINT` types + same index. **No foreign keys to `runs`** — time-series correlation is a query-time JOIN, not a referential constraint. |
| **`MintSampleID` helper** | ✅ `smp_<16hex unixnano><8hex rand>` — mirrors `MintChannelMessageID`. Sortable lexicographically by sample time; collision-safe within a single nanosecond via the 4-byte random suffix. |
| **Bounded retention** | ✅ Sweeper goroutine deletes rows older than `LOOMCYCLE_METRICS_RETENTION_DAYS` (default 7) at `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` cadence (default 15 min). Set retention=0 OR sweep interval=0 to disable (table grows unbounded). |
| **Consecutive-failure rate-limited logging** | ✅ Sampler tracks a failure counter. Logs loudly on the first store-write error or `/proc` read error, then every 10th. Prevents log flood on a wedged disk / disconnected Postgres pool / hardened-container `/proc` filter. Successful write resets the counter + emits a recovery log line. |
| **5 new env vars** | ✅ `LOOMCYCLE_METRICS_ENABLED` (default OFF; default-on planned for v0.9.x), `LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS` (default 5000; min-clamp 1000 to prevent write-storms from a typo'd `=50`), `LOOMCYCLE_METRICS_RETENTION_DAYS` (default 7), `LOOMCYCLE_METRICS_COLLECT_SYSTEM` (default OFF — Linux only), `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` (default 900000). Documented in `.env.example` with storage estimate (~210 MB/week steady-state at defaults). |
| **`cancel.Registry.ListAll()`** | ✅ General-purpose accessor returning a snapshot of every live entry regardless of user. **Not consumed by the sampler in v0.8.x** (the sampler uses `Semaphore.Stats()` for its active-runs gate); shipped as a forward-compat addition for future cross-cutting consumers with its own test coverage. |
| **Test coverage** | ✅ 28 new tests: 6 storetest contract tests (auto-run on sqlite + postgres — write+query round-trip, sweep idempotency, run-summary empty/with-samples/in-flight/not-found), 5 sampler unit tests (idle skip, write on active, graceful store error with rate-limited log, nil store, recovery counter reset; uses embedded-`store.Store`-interface fake for forward-compat against future Store additions), 8 `/proc` parser unit tests (fixture-based so they run on macOS CI too), 9 HTTP handler tests (503-when-disabled, samples round-trip + cursor, run-summary 404 + happy path, summary period bucketing, validation errors), 2 cancel registry tests. 37 packages green; race-detector clean on the 5 changed packages. |
| **Production-validated** | ✅ Deployed to operator's TrueNAS VM 2026-05-13. First exercised by an employer-profiler run that spawned company-researcher + 2 injection-judge sub-agents; captured 31 samples revealing loomcycle's per-process footprint at 21–33 MB RSS across the entire 3-way concurrent run tree. Per-run peak RSS for the 154-second orchestrator: 33 MB. |

## What's in v0.8.10

| Surface             | Status |
|---------------------|--------|
| **Gemini schema sanitizer (`$ref` + combinators)** | ✅ `sanitizeGeminiSchema` rewritten in `internal/providers/gemini/driver.go`. Inlines `$ref` (cycle-safe via per-path visited-set; diamond refs each inline independently; cycles emit `{}`; unresolved refs emit `{}`). Collapses `allOf` / `oneOf` / `anyOf` by **merging** ALL variants' `properties` + `required` into the parent (an earlier first-variant-wins draft was caught in code review — it silently dropped every discriminated-union variant past the first, which was exactly the bug the fix targeted). Type-conflict defense skips structural fields of variants with conflicting `type:` (e.g. `oneOf[object, array]` would otherwise produce a schema MORE broken than the input). Fixes `400 INVALID_ARGUMENT` rejection of Zod-shape MCP tool schemas. (PR #86) |
| **Realistic-MCP regression test** | ✅ `TestSanitizeGeminiSchema_RealisticMcpSchema` mirrors a Zod-generated `discriminatedUnion` + nested `$defs` + `additionalProperties` at multiple levels. Asserts NO banned key (`$ref`, `$defs`, `definitions`, `oneOf`, `anyOf`, `allOf`, `additionalProperties`, `$schema`, `$id`) leaks through AND both discriminated-union variants' payload properties survive. |
| **SQLite migration ordering fix** | ✅ `internal/store/sqlite/sqlite.go migrate()`. The v0.8.6 migration created `channel_messages_by_visible` index in the first `stmts` loop, BEFORE the `addColumns` ALTER block. Fresh deploys worked because the `CREATE TABLE IF NOT EXISTS channel_messages (...visible_at...)` declared the column up front; on an UPGRADE from v0.8.4/v0.8.5 the existing table had no `visible_at` and the CREATE INDEX failed with `SQL logic error: no such column: visible_at`. CI never caught this (every test run uses a fresh DB). Fix: moved the CREATE INDEX into `addIndexes` (which runs AFTER `addColumns`). Postgres unaffected. (PR #87) |
| **Upgrade-path regression test** | ✅ `TestMigrate_UpgradeFromV084ChannelMessages` simulates the upgrade path by hand-creating a v0.8.4 schema, then re-opening through `migrate()`. Pre-fix fails with the exact production error message; post-fix asserts both columns added, by_visible index created, and legacy `visible_at` backfilled from `published_at`. |
| **Both fixes consolidated** | ✅ v0.8.9 shipped the schema sanitizer; v0.8.10 added the sqlite migration fix that became necessary when deploying v0.8.9 from a v0.8.4 schema. Effectively v0.8.10 is the first release that's deployable to existing v0.8.4 / v0.8.5 sqlite-backed installations. |

## What's in v0.8.9

| Surface             | Status |
|---------------------|--------|
| **Gemini schema sanitizer (initial pass)** | ✅ See v0.8.10 above — v0.8.10 ships the consolidated description because v0.8.9 was followed by v0.8.10's sqlite migration fix within hours and the two are typically discussed together. v0.8.9 alone is deployable on a fresh (no prior `channel_messages` table) install. (PR #86) |

## What's in v0.8.8

| Surface             | Status |
|---------------------|--------|
| **`Context.help` op (tenth op on the Context tool)** | ✅ Returns a topic index when called without `topic` (`{topics: [{name, description, source}], count, hint}`); returns the full markdown body when called with `topic=<name>` (`{name, description, content, source}`). Unknown topic surfaces the available list in the error so the model can self-correct in one round-trip. |
| **Five bundled topics** (embedded via `//go:embed`) | ✅ `loomcycle` (intro to runtime + tool surface), `scopes` (agent/user/global isolation model across Memory + Channel), `subagents` (Agent sync spawn vs Channel async handoff; recursion cap; `def_id` pinning; cross-name pinning refusal), `experimentation` (the v0.8.5 fork → spawn → submit → aggregate → promote/retire/rollback loop), `system-channels` (the v0.8.6 `_system/*` namespace, admin endpoint, deferred publish). |
| **Filesystem overlay** | ✅ `LOOMCYCLE_HELP_ROOT` points at a directory of `<name>.md` files. Files with names matching bundled topics REPLACE them; new names extend the set. Symlinks under the help root are **refused** with a log line (trust-boundary protection — a stray `escape.md` symlink would otherwise let an operator exfiltrate any file the loomcycle process can read into the topic body the model sees). Per-file parse errors are **soft-skipped** so one malformed operator topic doesn't kill the runtime — bundled defaults remain intact. |
| **Frontmatter contract** | ✅ Standard Claude-Code-compatible YAML frontmatter. `name:` (must match filename stem) + `description:` (the one-liner shown in the index) are required; everything after the closing `---` is the body. Missing/mismatched name, missing description, or empty body refuses the topic at load time (bundled = fatal; operator = soft-skip). |
| **Wiring + tests** | ✅ New `internal/help/` package (loader + bundled FS + 16 unit tests). `Help *help.Set` field on Context built in `cmd/loomcycle/main.go` at boot; boot log emits `help: loaded N bundled topics (no LOOMCYCLE_HELP_ROOT overlay)` or `help: loaded N topics (filesystem overlay at <path>)`. 4 unit tests for `execHelp` (nil refusal, index mode, detail mode, unknown topic). Race-detector clean. Runtime smoke at `test/runtime/context-help/` passes against `gemini-2.5-flash` — the agent reads the index, calls back with `topic=scopes`, and quotes a phrase from the body. |
| **Schema update** | ✅ Context tool's op enum is now: `self` / `tools` / `doc` / `permissions` / `agents` / `lineage` / `evaluations` / `channels` / `history` / `help` (ten total). New top-level `topic` string field on the input schema. The default-add behaviour from v0.8.7 still applies — every agent gets Context auto-attached at config-load. |
| **`.env.example`** | ✅ Documents `LOOMCYCLE_HELP_ROOT` with the frontmatter contract + override semantics. |

## What's in v0.8.7

| Surface             | Status |
|---------------------|--------|
| **`Context` built-in tool — runtime introspection** | ✅ Read-only; no mutations, no network, no side effects. Nine ops on a single discriminated `op` field (same shape as Memory / Channel / AgentDef / Evaluation): `self` (identity bundle from `RunIdentity` + `AgentName` ctx-keys), `tools` (post-filter tool catalog with closed-set side-effect classifier — `pure` / `state` / `network` / `filesystem` / `privileged` / `unknown`), `doc` (input schema + description for one tool by name; refuses outside the per-run allowlist — no doc leak), `permissions` (bundle of every policy ctx-key — `allowed_tools`, `host_policy`, `memory`, `channels`, `agent_def_scopes`, `evaluation_scopes`, `history_scope`), `agents` (operator-declared agents from `cfg.Agents` with active `def_id` from the v0.8.5 substrate; optional `prefix` filter), `lineage` (walks ancestors via `parent_def_id` chain + descendants BFS; `depth` default 10, cap 100; **total-node cap 500** with `truncated` flag), `evaluations` (v0.8.5 `EvaluationAggregate` output — mean/median/min/max/latest + per-dimension + per-emitter-role; optional `include_lineage` walks ancestors), `channels` (operator-declared channels with per-caller publish/subscribe bools; wildcards surface separately in `publish_wildcards` / `subscribe_wildcards`), `history` (transcript events for the target agent — default caller's own; optional `event_types[]` filter + `limit` default 100/cap 1000; `truncated` is **honest under filter** by counting post-filter matches; gated by yaml `history_scope`). |
| **Default-add behaviour** | ✅ Every agent's `allowed_tools` gets `Context` auto-appended at config-load — missing introspection is a footgun for self-evolving agents. Opt-out is a single yaml line: `disable_context: true`. Duplicate-check is **case-insensitive** so `[context, Context]` doesn't sneak through. |
| **`history_scope` yaml gate (closed set, default-deny)** | ✅ `self` (caller's own run — practical default), `siblings` / `descendants` / `named:<n>` (reserved for v0.8.x — need `RunIdentityValue.ParentAgentID` plumbing), `any` (UNRESTRICTED — operator-trust grant for admin/debug agents). Default-deny: an agent without `history_scope` in its yaml cannot call `history` at all. |
| **Wire-protocol stability** | ✅ Schema enum locked at the ten ops listed above; v0.8.8 added `help` as the tenth. Adapters/SSE consumers can pattern-match on op names. |
| **Test coverage** | ✅ 30+ unit tests covering all nine ops (validation, allowlist filtering, ctx-key bundle assembly, lineage walk + truncation, history filter + truncation correctness). Runtime smoke at `test/runtime/context/` exercises four ops in one chained run against Gemini 2.5 Flash. |
| **PRs in v0.8.7** | ✅ #79 (self / tools / doc / permissions), #80 (agents / lineage / evaluations), #82 / #83 (channels / history + default-add + runtime smoke). |

## What's in v0.8.6

| Surface             | Status |
|---------------------|--------|
| **System channels (`_system/*` namespace)** | ✅ Operator-declared channels published by loomcycle-authoritative paths only. Three categories: (1) **Cadence** — `_system/heartbeat-1m`/`-5m`/`-1h` publish `{ts, version, uptime_s}` at fixed intervals via a dedicated `HeartbeatRunner` goroutine (skip-on-pause via shared `bgCtx`). (2) **Event-driven** — `_system/runtime-state` (pause/resume/restore transitions), `_system/provider-events` (fallback / cache-invalidated) — fire from internal subsystem hooks; no `period:` needed. (3) **Agent-publishable system channels** — `_system/alarms/critical`/`/warning`/`/info` are reserved-by-convention; operators publish via the admin endpoint or future alarm tools. |
| **`SystemPublisher` interface + `StorePublisher` impl** | ✅ Loomcycle-authoritative publish path that bypasses agent-tool ACL gates. Used by the heartbeat ticker AND the admin endpoint. Stamps `published_by_user_id` as `"_system"` (internal) or `"_admin"` (admin-endpoint). |
| **Tool-layer refusals** | ✅ Agents can NEVER publish to (a) channels with `publisher: system` OR (b) anything with the `_system/` prefix — even if an operator forgets to set `publisher: system` on a `_system/...` channel, the prefix itself is the defense-in-depth gate. |
| **Admin endpoint** | ✅ `POST /v1/_channels/_system/{name…}` — bearer-authed, accepts `{payload, deliver_at?}` body. Stamps `published_by_user_id="_admin"`. Use cases: external monitoring webhooks pushing alerts, ops dashboards, operators debugging from `curl`. |
| **Deferred publish (general — any channel)** | ✅ `Channel.publish` accepts optional RFC3339 `deliver_at`. Message stored immediately with `visible_at = deliver_at`; subscribers + `peek` filter `WHERE visible_at <= now()`. In-process `time.AfterFunc(visible_at)` scheduler wakes long-poll subscribers exactly at delivery time; bounded by `LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED` (default 10000). If the scheduler is over-cap or the process restarts mid-defer, deferred messages still get delivered on the next periodic poll — the scheduler is a latency optimisation, not a correctness mechanism. **TTL counts from `published_at`, NOT from `deliver_at`** — a 1-hour deferral with a 30-minute TTL means the message expires before becoming visible; size your TTL to cover both windows. |
| **Tuple cursor `(visible_at, msg_id)`** | ✅ Cursor format changes from `msg_<hex>` to `cur_<vh>_<msg_<…>>`. Pure msg_id ordering would silently skip deferred messages once a subscriber progressed past their publish-time id; the tuple ordering aligns the read path with delivery order. **Clean cursor break** — the 0005 migration truncates `channel_cursors` (v0.8.4 only shipped two weeks earlier, no production cursor state worth preserving). Subscribers replay from oldest on first subscribe after upgrade. |
| **Audit column** | ✅ New `channel_messages.published_by_user_id` populated from `RunIdentity` for agent publishes, `"_system"` for internal publishes, `"_admin"` for admin-endpoint publishes. Audit queries can distinguish operator + system + agent activity without grepping logs. |
| **Config validation** | ✅ `publisher: system` + `period:` rules enforced at config-load. `_system/` prefix is reserved (operator-only declaration; agents can never publish regardless of `publisher:` setting). |
| **Standard yaml** | ✅ `loomcycle.example.yaml` ships with the canonical heartbeat / alarm / runtime-state / provider-events channel set commented for operators to uncomment. |
| **PRs in v0.8.6** | ✅ #74 (deferred publish + tuple cursor), #75 (system publisher + admin endpoint), #76 / #78 (heartbeat ticker + runtime smoke). |

## What's in v0.8.5

| Surface             | Status |
|---------------------|--------|
| **`AgentDef` built-in tool — 6 ops** | ✅ `create` / `fork` / `get` / `list` / `promote` / `retire`. Single discriminated `op` field. Static `cfg.Agents` names are inviolate — must `fork`, never `create`. **AllowedTools ceiling is non-negotiable**: forks may NARROW the tool set, never widen; operator-blessed root is the permanent capability ceiling enforced via 100-hop cycle-guarded lineage walks. Per-agent yaml `agent_def_scopes` gates `self` / `descendants` / `named:[...]` / `any`, default-deny. |
| **`Evaluation` built-in tool — 5 ops** | ✅ `submit` / `get` / `list_for_run` / `list_for_def` / `aggregate`. Score model: required scalar (RL lingua franca) + optional `dimensions` map + optional `judgement` JSON + optional `rationale` text. **`emitter_role` derived server-side** from caller's `RunIdentity` vs target run's identity (`self` / `parent` / `external` / `unrelated`) — the model can't lie about who scored what. `sibling` collapses to `unrelated` today (RunIdentityValue lacks emitter ParentAgentID); `submit_siblings` scope is reserved-but-inert; `submit_any` is the escape hatch. Per-agent yaml `evaluation_scopes` gates submit roles + read ops. |
| **Versioned `agent_defs` + lineage** | ✅ Append-only `agent_defs` (UUID `def_id`, monotonic `version` per `name`, `parent_def_id` for lineage, `bootstrapped_from_static` flag). `agent_def_active` pointer table for "which version a name resolves to." Promote/retire flip pointers — they never rewrite definition rows. Postgres `pg_advisory_xact_lock(hashtextextended('agent_def:' || name, 0))` serialises version allocation per name; sqlite uses pinned-conn + `BEGIN IMMEDIATE`. Tested under contention: 250 parallel forks → exactly versions 1..250 with no gaps or duplicates on both backends. |
| **Sub-agent `def_id` pinning** | ✅ Optional `def_id` on the `Agent` tool input. `runSubAgent` overlays the row onto static `cfg.Agents` for that one sub-run (Model/Tier/Provider/Effort apply correctly via `resolveAgentDef`). `agent_def_id` persisted on the sub-run row + denormalised onto evaluations at submit time — aggregate queries downstream automatically partition by def. **Substrate policy fields are NEVER in the overlay surface** so forks can't widen their own gates. **Cross-name pinning refused** — passing a `def_id` whose row was created for a different agent name returns "cross-name pinning refused"; prevents namespace hijack. |
| **Selection stays policy** | ✅ Loomcycle does NOT auto-promote based on score. Agents (or operator orchestrators) call `Evaluation.aggregate` + `AgentDef.promote` per their own policy — max, GA, PPO, RLHF, whatever. Keeping policy out of the runtime is what lets it host arbitrary selection strategies. |
| **Migrations (additive)** | ✅ Postgres: `0006_agent_defs`, `0007_runs_agent_def`, `0008_evaluations`. SQLite: idempotent CREATE TABLE + `ALTER TABLE runs ADD COLUMN agent_def_id TEXT`. |
| **PRs in v0.8.5** | ✅ #65 (storage + locks + aggregate kernel), #66 (config + ctx-key plumbing), #67 (AgentDef tool), #68 (Evaluation tool), #71 (runtime smoke), #72 (sub-agent def_id pinning). |

## What's in v0.8.4

| Surface             | Status |
|---------------------|--------|
| **`Channel` built-in tool** | ✅ Persistent inter-agent message bus. Five ops on a discriminated `op` field: `publish` (append JSON payload to a named channel; ACL-gated), `subscribe` (drain up to N new messages + return a cursor; optional `wait_ms` long-poll), `ack` (explicitly commit a cursor; rejects regressions via `ErrChannelCursorRegression`), `peek` (non-consuming debug read), `list_channels` (informational ACL dump). Subscribe is at-most-once-by-default (commits `next_cursor` on return); agents wanting at-least-once / crash safety use `peek` → process → `ack`. Same single-discriminated-`op` shape as Memory. Sub-agents inherit the parent's ACL via ctx (mirror of `WithMemoryPolicy` / `WithHostPolicy`). |
| **Storage-layered backend** | ✅ Messages persist to `store.Store` via two new tables: `channel_messages` (TEXT id ULID-style `msg_<unixnano><rand>`, payload JSONB on Postgres / TEXT on SQLite, expires_at) and `channel_cursors` (per-subscriber committed position). Cursor scope mirrors Memory: `agent` (one cursor per agent name), `user` (per user_id), `global` (one shared cursor). Additive `0004_channels.up.sql` Postgres migration; idempotent CREATE TABLE on SQLite. Storetest contract suite: 11 subtests run on both backends — publish/subscribe ordering, cursor monotonicity, TTL filter at read, max_messages trim, scope isolation, replay via `cur_0`, ack-regression rejection. |
| **In-process notification Bus** | ✅ New `internal/channels/` package. `Bus.Notify(channel)` wakes any in-process subscribers blocked in `Bus.Wait(ctx, channel, timeout)`. Subscribe with `wait_ms > 0` queries storage, then blocks on the bus until a publish lands or the timeout fires — sub-millisecond latency for same-process consumers; cross-process subscribers fall back to polling. 7 race-detector-clean tests (notify wakes, timeout returns false, ctx cancel returns early, fan-out, channel isolation, no-timeout-no-wait, stress under concurrent notify+wait). |
| **Operator-yaml ACL** | ✅ New top-level `channels:` block declares the namespace (per-channel `scope` / `default_ttl` / `max_messages` / `semantic`). Per-agent `channels: {publish: [...], subscribe: [...]}` allowlists name channels with optional trailing `/*` wildcard (`findings/*` matches `findings/alpha` but NOT `findings`; mid-string globs rejected at config-load so an operator typo can't grant `*` access). Same trust model as `allowed_tools` + `memory_scopes`. Validation: every ACL entry must reference a declared channel; wildcards with no matches at load time are rejected. |
| **Lossy-on-overflow bounded storage** | ✅ Each channel declares `max_messages`; publishes that push the per-(channel, scope, scope_id) count past trim OLDEST rows inside the same txn. Publisher never blocks — the v0.8.4 RFC's central trade-off (cost cap → never starve the producer). The publish result includes `dropped_oldest: N` so the tool layer (and future audit events) sees the overflow signal. 0 = unbounded. |
| **Three new env vars** | ✅ `LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES` (per-publish payload cap, default 64 KB), `LOOMCYCLE_CHANNELS_SWEEP_MS` (TTL reaper cadence, default 15 min), `LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS` (max `wait_ms` allowed on subscribe, default 30 s). All have sensible defaults; zero disables. |
| **Operator visibility** | ✅ Boot log emits `channels: configured N — channel-a / channel-b / ...` (mirror of `user_tiers:` line shape). Sweeper goroutine logs per-sweep delete count when > 0. `loomcycle.example.yaml` ships with two canonical channels — `findings` (scope: agent, semantic: queue, 24h TTL, 10k max) and `alerts` (scope: global, semantic: broadcast, 1h TTL, 1k max) — plus two example agents (`researcher` publishes, `analyst` subscribes) demonstrating the canonical handoff pattern. |

## What's in v0.8.3

| Surface             | Status |
|---------------------|--------|
| **Provider split: `ollama` + `ollama-local`** | ✅ Hosted ollama.com (Bearer auth via `OLLAMA_API_KEY`) is now `ollama`; local-network Ollama (no auth, default `http://localhost:11434`) is now `ollama-local`. One driver package serves both — same `/api/chat` wire shape; only the auth header + base URL differ. Existing deploys with `OLLAMA_BASE_URL=http://localhost:11434` keep working unchanged (the env var now drives `ollama-local`). Two new env vars: `OLLAMA_API_KEY` + optional `OLLAMA_CLOUD_BASE_URL`. Library `defaultLibraryPriority` becomes `[ollama-local, deepseek, openai, anthropic, ollama]` — workstation at the floor, hosted ollama after the paid clouds. (PR #55) |

## What's in v0.8.2

| Surface             | Status |
|---------------------|--------|
| **`user_tier` policy + resolver overlay** | ✅ Operator-defined named user-tier policies in `loomcycle.yaml` (`user_tiers:` block) — each tier carries its own `provider_priority`, per-task-tier `tiers`, `fallback_on_error` switch, and `max_fallback_attempts` cap. Runs carry `user_tier` per-request via `POST /v1/runs` (and `POST /v1/sessions/{id}/messages`); empty falls through to the required `default` entry; unknown name → 400. The resolver overlays the tier's policy between library defaults and per-agent overrides; `agent.providers ∩ user_tier.provider_priority` empty → `ErrTierAgentNotAvailable` (distinct from outage so clients render "upgrade required"). Sub-agents inherit the parent's `user_tier` via ctx. New `runs.user_tier` column (additive migration on both SQLite + Postgres) drives cost retros + compliance audit. (PR #52) |
| **Runtime provider fallback** | ✅ When a provider call returns a retryable error (429/5xx/network/v0.8.1 stream-idle), the loop swaps to the next-in-queue provider within the user_tier's candidate list and continues the iteration. Five-bucket error classifier in `internal/providers/errclass.go` distinguishes retryable from permanent (400/401/403/422) so config errors don't cascade through every provider's quota. Cumulative 3-attempt budget per run; per-tier `fallback_on_error: false` opts free tiers out of the cascade (cost-cap semantic — 429 returns error to client, no climb to paid providers). New typed events `EventProviderFallback` (with structured `FallbackInfo` payload) and `EventCacheInvalidated` (fired only on `anthropic → other` since Anthropic is the only provider with operator-controlled `cache_control` today). (PR #53) |
| **Per-tier policy in operator yaml** | ✅ `user_tiers:` block ships with five canonical tiers in `loomcycle.example.yaml`: `default` (back-compat for v0.7.x clients — mirrors the library defaults), `free` (ollama-only, no cascade — cost-cap shape), `low` (deepseek + anthropic, cascade on), `medium` (openai + anthropic + deepseek, cascade on), `high` (anthropic-only, no cascade — premium SLA). Each tier carries its own `fallback_on_error` posture. The "default" entry is required when the block is populated; validation rejects unknown providers/tiers and negative `max_fallback_attempts`. |
| **Per-run audit marker** | ✅ `runs.user_tier` column on both backends with the additive `0003_user_tier.up.sql` Postgres migration. Compliance + cost-retrospective queries facet by tier without grepping logs. The boot log emits `user_tiers: configured N — default / free / low / medium / high` so operators see what's available at startup. |

## What's in v0.8.1

| Surface             | Status |
|---------------------|--------|
| **Provider streaming timeouts** | ✅ Replaced the 5-min wall-clock `http.Client.Timeout` with a header + per-byte idle pair. `Transport.ResponseHeaderTimeout` caps time-to-first-byte (default 60 s); a body wrap resets a timer on each Read and cancels the request context on stall (default 90 s). Long but actively-emitting final-turn responses (e.g. job-searcher emitting a 25-position ingest payload) now complete instead of getting cut mid-stream. Two operator knobs: `LOOMCYCLE_PROVIDER_HEADER_TIMEOUT_MS` / `LOOMCYCLE_PROVIDER_IDLE_TIMEOUT_MS`. All five provider drivers updated; `streamhttp` package + 8 unit tests; `-race` clean. (PR #47) |
| **Lazy MCP retry on first agent call** | ✅ MCP servers that failed initial handshake at boot (peer down, slow to start, or broken at the time loomcycle started) used to stay marked `skipped` for the lifetime of the loomcycle process — operators had to restart loomcycle by hand once the peer recovered. Now the dispatcher carries an optional `FallbackFunc` (set in `cmd/loomcycle/main.go`); a tool name matching `mcp__<server>__<tool>` for a configured-but-skipped server triggers one fresh `pool.Get` for that server on the agent's call path. On success, the server's tools are memoised and dispatched; the operator-visible log line is `mcp[<server>]: lazy-registered N tool(s) on first agent call (was skipped at boot)`. Subsequent calls hit the cache without re-handshaking. The pool's existing `entry/ready` channel coalesces concurrent first-touches to a single underlying handshake (50-way concurrency test pinned). Peer restarts no longer require a loomcycle restart — addresses the "components restart independently in a server environment" failure mode. (PR #48) |
| **Agent directory discovery** | ✅ New `LOOMCYCLE_AGENTS_ROOT` points at a directory of flat `<name>.md` files. Each file's YAML frontmatter is the base `AgentDef`; the body becomes `system_prompt`. The yaml `agents:` map remains an OPTIONAL override layer — yaml entries with the same name override discovered fields per-field (yaml-as-override). Mixed-mode, MDs-only, and yaml-only deployments all supported. Frontmatter is flat top-level keys (`name` / `description` / `tools` / `model` / `tier` / `models` / `effort` / `max_tokens` / `skills` / `memory_scopes` / `memory_quota_bytes` / `providers` / `allowed_tools` / `system_prompt_file`); accepts both Claude Code's `tools: A, B, C` (comma-string) and loomcycle's `allowed_tools: [A, B, C]` (yaml list); `allowed_tools` wins when both present. Single source of truth for operators maintaining `.claude/agents/*.md` for Claude Code AND a corresponding loomcycle `agents:` block. (PR #49) |

## What's in v0.8.0

| Surface             | Status |
|---------------------|--------|
| **`Memory` built-in tool** | ✅ Persistent agent-scoped key/value storage that survives across runs and sessions. Five ops behind one tool: `get` / `set` / `delete` / `list` / **`incr`** (atomic counter). Two scopes: `agent` (yaml-keyed; cross-run, shared across users) and `user` (user_id-keyed; cross-agent, per end-user). Backed by a new `memory` table on both SQLite and Postgres adapters. (PR #45) |
| **Per-agent yaml policy** | ✅ `memory_scopes: [agent, user]` is a default-deny allowlist — `Memory` in `allowed_tools` is necessary but not sufficient. Optional `memory_quota_bytes` per-agent override of the global `LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES` cap. Sub-agents get their OWN policy from yaml — the parent's `memory_scopes` does NOT cascade. (PR #45) |
| **Web UI Memory page** | ✅ `/ui/memory` — three-pane browser: scope picker → scope_id list with key counts and byte totals → keys with prefix filter → entry detail with pretty-printed JSON, timestamps, and TTL. Polls the new `/v1/_memory/*` admin endpoints on a 5 s tick. (PR #45) |
| **Admin API for Memory** | ✅ Four read-only routes — `GET /v1/_memory/scopes`, `/scopes/{scope}`, `/scopes/{scope}/{scope_id}/keys`, `/scopes/{scope}/{scope_id}/keys/{key...}`. Bearer-authed via the existing middleware. The `{key...}` multi-segment route handles slashed keys like `events/2026-05-09T10:00`. (PR #45) |
| **Concurrency hardening** | ✅ Atomic increment correctness verified by a 100-goroutine regression test on both backends. Caught and fixed two real lost-update races at review time: SQLite `BeginTx(nil)` is DEFERRED (fix: pinned connection + raw `BEGIN IMMEDIATE`); Postgres `SELECT FOR UPDATE` doesn't lock absent rows (fix: `pg_advisory_xact_lock` keyed by hash of the (scope, scope_id, key) tuple). (PR #45) |
| **Pre-existing host-policy fix** | ✅ `handleMessages` (session continuation path) had been missing `tools.WithHostPolicy` on its loop ctx since v0.4.0 — sub-agents from continuations fell back to the operator's static allowlist instead of the caller's narrowed list. Fixed alongside the new Memory ctx values. (PR #45) |

## What's in v0.7.4

| Surface             | Status |
|---------------------|--------|
| **Web UI agent name + content fixes** | ✅ Run list now shows the YAML-declared agent name (`qa-agent`, `company-researcher`) instead of just the UUID. Agent detail header reads from the corrected wire shape (model + tokens + duration). Transcript event panels now render actual content (text, tool calls, tool results, errors) — collapsed-by-default with a one-line summary, click to expand for full text + tool params + pretty-printed JSON. (PRs #41, #42) |
| **User picker dropdown** | ✅ New `GET /v1/_users` admin endpoint surfaces distinct user_ids that have runs in the store, with running / total counts + last-active timestamp. The Web UI top bar swaps the freeform user_id input for a dropdown — operators no longer need to know the UUID up front. Manual override (✎ button) preserved for picking a user who has no runs yet. (PR #40) |
| **Gemini config validation hotfix** | ✅ v0.7.2 wired the Gemini driver into the resolver but missed adding `gemini` to the config validator's allowlist; operators with `provider: gemini` rows in their yaml saw startup fail. Fixed. (PR #39) |

## What's in v0.7.3

| Surface             | Status |
|---------------------|--------|
| **Embedded read-only Web UI** | ✅ React 19 + Vite 7 + TypeScript SPA at `/ui`. Two pages: run list (parent → children tree, status filter, auto-refresh every 3 s) and per-agent detail (event log: text / thinking / tool_call / tool_result / error / retry / done; auto-refresh every 1.5 s for active runs; cancel button). No new wire endpoints — the SPA reuses the existing `/v1/users/{user_id}/agents`, `/v1/agents/{agent_id}`, `/v1/sessions/{id}/transcript`, `/v1/agents/{agent_id}/cancel` routes. |
| **Bearer-in-cookie auth** | ✅ Operator visits `/ui?token=<bearer>` once; server sets a `loomcycle_session` HttpOnly cookie and 302s back. Subsequent /v1 calls authenticate via the cookie (same-origin fetch). The existing `Authorization: Bearer …` header path keeps working unchanged for adapters / curl / SDKs — bearer wins on precedence so a stale cookie can't mask a deliberate request. |
| **Build pipeline** | ✅ `make build-ui` runs `npm install + npm run build` and writes the production bundle to `internal/webui/dist/` (embedded via `go:embed`). `make build-all` does both. A fresh checkout without npm toolchain still compiles via Go alone (a committed `.gitkeep` placeholder); `/ui` then returns 503 with a `ui_not_built` code as the diagnostic. |

## What's in v0.7.2

| Surface             | Status |
|---------------------|--------|
| **Google Gemini provider** | ✅ Fifth backend driver in `internal/providers/gemini/`. Speaks Gemini's `generateContent` API: model name in URL path (not body), `x-goog-api-key` header auth, SSE streaming via `?alt=sse`. Tool dispatch maps loomcycle's `tool_use` / `tool_result` to Gemini's `functionCall` / `functionResponse` content parts. |
| **Effort hint translation** | ✅ `effort: low \| medium \| high` maps to `generationConfig.thinkingConfig.thinkingBudget` on gemini-2.5-flash / gemini-2.5-pro: `low` → 0 (disable), `medium` → 2048, `high` → 8192 (clamped to `max_tokens - 1024` when needed). Same vocabulary as Anthropic / OpenAI — no per-provider effort dialect. |
| **Resolver matrix integration** | ✅ Excluded when `GEMINI_API_KEY` is unset; probed at startup and on the periodic re-probe with the same 5 s deadline as the others. Per-agent yaml: `provider: gemini` and `model: gemini-2.5-flash` (or any model the wire `/v1beta/models` returns). |
| **Vertex AI deployments** | ✅ Optional `GEMINI_BASE_URL` overrides for Vertex AI Gemini gateways (production deployments routing through GCP project quotas instead of the public AI Studio API). |

## What's in v0.7.1

| Surface             | Status |
|---------------------|--------|
| **`EventThinking` event type** | ✅ Live streaming of model reasoning as a typed event distinct from `EventText`. Anthropic from `thinking_delta` content blocks; OpenAI / DeepSeek from `delta.reasoning_content`; Ollama from `message.thinking`. `EventDone.Reasoning` still carries the consolidated trace for next-turn echo (DeepSeek roundtrip). |
| **Tool-use hooks** | ✅ Operator-supplied middleware around tool dispatch via `POST /v1/hooks`. Selectors filter by `(agent, tool, phase)`; per-`(owner, name)` idempotent registration prevents cascading on app restart. Fail-open default (telemetry hooks don't block); fail-closed available for security-shaped hooks. See [`docs/TOOLS.md`](docs/TOOLS.md). |
| **Resolver Snapshot endpoint** | ✅ `GET /v1/_resolver` exposes the in-process availability matrix as JSON, bearer-authed. 503 with `resolver_unavailable` in the brief degraded-startup window so dashboards distinguish "matrix not available" from "matrix is empty". |
| **Parallel tool dispatch** | ✅ The agent loop dispatched a turn's tool_calls serially — `Agent` fan-outs queued. New `executePendingTools` runs each in its own goroutine, default 8 concurrent, `LOOMCYCLE_TOOL_PARALLELISM` to override. |
| **SSE wire-level keepalive** | ✅ Long-lived agent streams emit `: keepalive\n\n` comment frames every 20 s by default. Closes the opaque `TypeError: terminated` undici reports when networks with idle-connection timeouts (Tailscale, NAT routers) drop a silent stream. `LOOMCYCLE_SSE_KEEPALIVE_MS` to override; 0 disables. |
| **Per-token text coalescing** | ✅ OpenAI / DeepSeek streaming text deltas accumulate into 64-byte chunks. Closes the "every word on its own line" cosmetic noise DeepSeek's tokenizer produced. |
| **Ollama qwen3 tool-call recovery** | ✅ Both JSON-shape (`{"name":"...","arguments":{...}}`) and bracketed-markdown (`[tool_use: name]\n{args}`) forms now synthesize `EventToolCall` so the loop iterates instead of terminating with the markup as the final answer. |
| **DeepSeek thinking-mode roundtrip** | ✅ DeepSeek V4 Pro / deepseek-reasoner returns `reasoning_content` alongside `content`; the API requires it echoed back on subsequent turns. The OpenAI driver captures it on `EventDone.Reasoning`; the request builder serialises it back when the assistant Message carries one. |

## What's in v0.7.0

| Surface             | Status |
|---------------------|--------|
| **Tier-based resolution** | ✅ Agents declare `tier: low \| middle \| high` instead of pinning a specific model. Resolver picks `(provider, model)` against a live availability matrix. Per-agent `providers:` and `models:` overrides cover asymmetric pinning. Explicit pins from v0.6.x continue to work. |
| **Live `/v1/models` probes** | ✅ Each driver implements `Probe` + `ListModels`. Startup probes run in parallel with a 5s deadline; periodic re-probe runs every 15 min (configurable up to 1h via `LOOMCYCLE_RESOLVE_PROBE_INTERVAL_MS`). |
| **`Excluded` flag**  | ✅ Providers without API keys are explicitly marked excluded in the matrix — distinct from "probe attempted, failed". Visible in `Resolver.Snapshot()` for dashboards. Startup logs surface the state. |
| **Reactive stall feedback** | ✅ Loop calls `MarkStalled` on driver errors (5xx after retry, mid-stream errors). Resolver skips stalled `(provider, model)` pairs until next probe revives. `ctx.Err()` guards prevent user-cancellations from polluting the matrix. |
| **Per-driver effort hint** | ✅ Agent yaml: `effort: low \| medium \| high`. Anthropic → `thinking.budget_tokens` (haiku always skips); OpenAI → `reasoning_effort`; DeepSeek inherits OpenAI; Ollama is a no-op. Loop logs once per Run when effort is dropped. |

## What's in v0.6.0

| Surface             | Status |
|---------------------|--------|
| **DeepSeek provider** | ✅ Wraps the OpenAI driver with the DeepSeek base URL pre-baked. Per-agent yaml: `provider: deepseek`. Set `DEEPSEEK_API_KEY`; optional `DEEPSEEK_BASE_URL` for self-hosted OpenAI-compatible mirrors (vLLM, etc.). |
| **OpenAI `Usage.Model` fix** | ✅ Driver now captures the wire-resolved model alias from the streamed chunk envelope, so `runs.model` populates for every OpenAI-compatible run (OpenAI itself, DeepSeek, vLLM). Same regression class as the v0.4 anthropic fix; latent until the DeepSeek live test surfaced it. |
| **Ollama live integration tests** | ✅ Three tests (probe, chat, tool call) gated by `OLLAMA_TEST_BASE_URL`. Validated against qwen3:14b on RTX 5080 (16GB VRAM) end-to-end as the offline / cost-floor backend. |
| **Constant-time bearer compare** | ✅ New `internal/auth.CompareBearer` (sha256+CTC) replaces raw `subtle.ConstantTimeCompare` on both HTTP and gRPC. Closes a length-leak side channel that the stdlib documents but doesn't fix. |

**Provider routing intent (jobs-search-agent first):** Anthropic for user-sensitive paths · DeepSeek for high-volume public data · Ollama (local llama) for offline / cost floor · OpenAI for general use / prototyping. See [`docs/PLAN.md`](docs/PLAN.md#v060--earlier) for the full rationale and rollout history.

## What's in v0.5.5

| Surface             | Status |
|---------------------|--------|
| **gRPC server**      | ✅ Opt-in via `LOOMCYCLE_GRPC_ADDR`. All seven RPCs mirror the HTTP+SSE surface 1:1 (`Run`, `Continue`, `GetAgent`, `CancelAgent`, `ListUserAgents`, `GetTranscript`, `Health`). Coexists with HTTP — same store, same cancel registry, same semaphore. See [`docs/GRPC.md`](docs/GRPC.md). |
| **Python adapter**   | ✅ `pip install loomcycle`. Async `LoomcycleClient` over `grpc.aio` covering all seven RPCs. PEP-561 `py.typed`. |
| **`internal/runner/`** | ✅ Wire-agnostic seam — HTTP server satisfies `runner.Runner`, gRPC server delegates to the same instance. |
| **Synthetic registration frames** | ✅ Wire-stable `session` + `agent` frame pair at the head of every Run/Continue stream so adapters capture `(agent_id, run_id, session_id, parent_agent_id)` without re-decoding the transcript. |

## What's in v0.5.0

| Surface             | Status |
|---------------------|--------|
| **Postgres backend** | ✅ Full `Store` adapter over `pgx/v5` + embedded `golang-migrate`. Same interface as SQLite; adapters share a contract suite so they can't drift. See [`docs/POSTGRES.md`](docs/POSTGRES.md). |
| **SQLite stays first-class** | ✅ Default backend; both adapters tested against the same behavioural contract suite. |
| **Heartbeat sweeper** | ✅ Periodic background goroutine marks runs whose process crashed mid-loop as `failed`. Default-on, env-tunable. |
| **Session-lock map GC** | ✅ Refcounted + idle-pruned; closes the v0.3.2 leak where `sessionLocks` grew monotonically. |
| **CLI subcommands**  | ✅ `loomcycle validate` · `agents list` · `health` · `migrate up\|down\|status` · `migrate sqlite-to-postgres` |
| **`make pg-up` / `pg-down`** | ✅ Local Postgres fixture for tests + dev. |

The bulk of v0.5.0 is operational: backbone you'll need before scaling past one replica. SQLite stays the default for compact installs.

## What's in v0.4.0

| Surface             | Status |
|---------------------|--------|
| **Providers**       | Anthropic ✅ · OpenAI ✅ · Ollama ✅ (tool-tuned models only). DeepSeek added in v0.6.0; Gemini in v0.7.2; Ollama-local split out in v0.8.3. |
| **Built-in tools**  | Read · Write · Edit · HTTP · WebFetch · WebSearch · Bash · **Agent** · **Skill** (Memory added in v0.8.0) |
| **MCP transports**  | stdio (pooled, auto-respawn) · HTTP (Streamable, SSE-aware) |
| **MCP startup retry** | Exponential backoff handshake on boot — handles peer-still-starting races |
| **LocalAPI gateway** | ⏳ scaffolded — useful for consumers that have an OpenAPI spec but don't want to stand up an MCP server. Not the v0.4 integration vehicle (jobs-search-agent migrated to the MCP-server pattern instead). |
| **Sub-agents**      | Agent built-in spawns child runs; depth-capped; parent host policy + identity inherit via ctx |
| **Skills**          | Approach A: static bundling at config-load (skill body concatenated into agent system prompt) |
| **Storage**         | SQLite (modernc.org, pure Go); sessions / runs / events tables; partial indexes for v0.4 sub-agent columns |
| **Concurrency**     | Global semaphore + bounded FIFO queue; backpressure → HTTP 429 |
| **Cancellation**    | Registry-based cancel API; cascades from parent to all children via `parent_agent_id` walk |
| **Adapters**        | TypeScript (`@loomcycle/client`) ✅ · Python ⏳ deferred (shipped in v0.5.5) |

> **v0.4.0 — released after end-to-end MCP integration with jobs-search-agent.** Two agents (`ats-filter`, `qa-agent`) now fetch context — and `qa-agent` also persists results — through typed `mcp__jobs__*` tools served by jobs-search-agent's own MCP server. This validates the runtime's MCP HTTP transport, host-policy inheritance, sub-agent retry, SSE response decoding, and Streamable-HTTP `Accept` handling against a real consumer. Per-agent migration in the consumer continues incrementally; the loomcycle surface is stable.
