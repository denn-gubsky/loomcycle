# Roadmap

This is the public roadmap. For decision history, regret notes, and per-version commit-by-commit details, see `~/work/loomcycle-internal/doc-internal/PLAN.md` (operator-side separate repo; migrated out of this tree as part of v0.8.15).

## Where we are

loomcycle has shipped through **v0.25.0**. The v0.17.0 multi-tenant-authorization capstone (RFC L) was followed by a substrate-completeness line: **v0.18.0** (idempotent `MCPServerDef` ingestion), **v0.20.0** (inline code-js bodies + Web UI + full runtime symmetry), **v0.21.0** (a non-secret metadata channel across all three trigger surfaces + a code-js run-budget overhaul), **v0.22.0** (tenant isolation extended from the state plane to the agent/skill/MCP/Schedule/Webhook *definition* plane — RFC N), **v0.23.0** (MCP-server hardening — concurrent stdio dispatch + bounded `spawn_run` + the single-runtime invariant / thin client, RFCs O/P/R, plus the RFC Q DeepSeek tool-content fix), **v0.24.0** (architecture-review hardening: the RFC N tenant axis completed across all 8 definition families incl. a per-tenant inbound-webhook route, the RFC P `spawn_run` timeout extended to the HTTP/`--upstream` transport, and agent interactive-config ACLs made content-identifying — F14), and **v0.25.0** (the agentic-ensemble release: a full manual-management Web UI console + the RFC S synchronization primitives — `Context op=time`, `Channel.await` / `Channel.broadcast` fan-in/out reaching the in-band tool + MCP + REST/gRPC/TS client twins, and `max_fires` self-retiring schedules). Per-version shipped detail now lives in [`REVISIONS.md`](../REVISIONS.md); the historical design-roadmap entries from the v0.8.x series are retained below for context. This section tracks the path forward.

## Shipped — v0.17.0 (OSS multi-tenant authorization, RFC L)

Originally scoped as the v1.0 capstone; it shipped as its own minor release so the v1.0 tag stays a pure hardening + distribution milestone. The single shared token is no longer the only way to authenticate: a substrate of bearer tokens (`OperatorTokenDef`), each bound to an **authoritative principal** — a `(tenant, subject, scopes)` resolved *from the token*, not trusted from the request body. Landed as a three-PR series (token substrate + store + CLI/audit → auth middleware + principal + identity threading + scope enforcement → cache/invalidation + docs), an adversarial-QA hardening pass (1 CRITICAL gRPC scope bypass + 4 HIGH), and a role-aware Web UI (token login + tenant workspace + super-admin tenant-focus). What it unlocked:

- **Token-per-principal.** A team or small-VPS service issues a distinct token per developer / app, each minted by loomcycle (CSPRNG, shown once) — callers stop sharing one omnipotent secret.
- **Authority-derived isolation.** The principal's tenant + subject drive boundaries that already existed — per-subject resource fairness and per-tenant memory / run isolation became *real* (they previously keyed on caller-asserted fields). The wire `tenant_id` / `user_id` are advisory, overridden by the token.
- **Scopes.** Narrow tokens (e.g. `runs:create` for an app key) vs admin tokens; default-deny from a documented catalog, enforced per HTTP route **and** per gRPC RPC.
- **Rotation + audit.** Two-token grace-window rotation; a file-based JSONL audit log of every token create / rotate / retire.
- **Zero-disruption upgrade.** `LOOMCYCLE_AUTH_TOKEN` keeps working (migrates in place via `operator-token create --copy-from-env`); single-operator deployments need no migration. Multi-tenancy is *available*, never *required*.

Enterprise-grade authorization — SSO (SAML/OIDC), RBAC roles, SCIM provisioning, signed / queryable audit logs, automated rotation policies, compliance evidence — is intentionally **out of scope for OSS** and lives in a separate enterprise edition built on the same token substrate. The OSS edition does enough for a 200-developer team; the enterprise edition does what passes a procurement security review.

## Planned — v1.0 (hardening + distribution)

With the multi-tenant-auth capstone shipped, v1.0 is a pure hardening + distribution milestone — no new primitives.

- **Distribution + bootstrapping.** A hardened first-run install story: multi-arch **Homebrew** formula + **Docker** images wired to the `init` / `doctor` flow and a sane default config, so `brew install` / `docker run` reaches a working sidecar without reading the docs first.
- **Claude Code plugin hardening.** The `claude-code-plugin-loomcycle` plugin (slash commands + skills + hooks over `loomcycle mcp`) gets a robustness pass — error surfaces, version-skew handling, clean bootstrap from the published binary.
- **Security + robustness + runtime-QA pass** across the v0.13–v0.17 surfaces (A2A interoperability, input webhooks, pluggable memory + the memory layer, the synthetic code provider, the multi-tenant-auth substrate), then the v1.0 tag.

**Beyond v1.0** (polish, unscheduled): a settings UI, an operator cookbook of deployment postures, broader distribution (Helm).

---

## v0.8.23 — earlier

**Status: shipped (2026-05-20).** **`SkillDef` + `AgentDef` on every wire surface.** PR #163 lifts the v0.8.22 SkillDef substrate (and the previously MCP-only AgentDef substrate) onto HTTP admin endpoints + gRPC RPCs + TS + Python adapter methods. Symmetric exposure across all five transports; same connector dispatch path under each wire.

**What's in v0.8.23 (vs v0.8.22):**

- **Connector interface extension.** `connector.Connector` gains `SkillDef(ctx, input) (ToolResult, error)`. HTTP server implements via the existing `dispatchBuiltin` pattern. Same architectural seam every other transport consumes.
- **MCP meta-tool.** New 26th meta-tool `skilldef` mirrors `agentdef`. The stdio MCP server's `operatorCtx` grants `SkillDefPolicy` with scope=[any] alongside the existing AgentDef grant.
- **HTTP admin endpoints (NEW for both tools).** `POST /v1/_agentdef` + `POST /v1/_skilldef` — bearer-authed, accept the op-discriminated JSON body the in-process tools accept, dispatch through the Connector with operator-trust ctx. Tool refusals (scope deny, empty body) map to 422 with `{code: "tool_refused", error, tool}`. AgentDef admin access was previously MCP-only.
- **gRPC RPCs (NEW for both tools).** New `AgentDef(SubstrateRequest)` and `SkillDef(SubstrateRequest)` RPCs on the Loomcycle service. Single op-discriminated method per tool; `SubstrateResponse { bytes output_json, bool is_error }`. Tool-level refusals come back as `is_error=true` on the response body; transport-level failures (auth, malformed input) come back as gRPC status codes.
- **TS adapter (NEW for both tools).** `LoomcycleClient` gains `agentDef(input)` + `skillDef(input)` methods. New typed error `SubstrateToolRefusedError` (extends `LoomcycleError`); `raiseFromResponse` disambiguates 422 by body shape (`code=tool_refused` → typed error; other 422s → existing `SnapshotVersionError`). 7 Vitest cases; total now 85 tests across 4 files. `@loomcycle/client` 0.8.20 → 0.8.23 (skipping .21 + .22 since the TS adapter didn't ship intermediate releases).
- **Python adapter (NEW for both tools).** `LoomcycleClient` gains `agent_def(input)` + `skill_def(input)` async methods. Same `SubstrateToolRefusedError` exception with a `tool` attribute. Server-side `INVALID_ARGUMENT` now maps to `InvalidArgumentError` (was bare `LoomcycleError`) — `code` attribute distinguishes server-side (`grpc.StatusCode.INVALID_ARGUMENT`) from client-side (`None`). 5 pytest cases; total now 77 tests. `loomcycle` 0.6.1 → 0.7.0 (minor bump for new public methods + typed error).
- **Substrate hotfixes.** Code review of the v0.8.22 substrate surfaced two production bugs, fixed in this release:
  1. `runSubAgent` bypassed `resolveSkillBodiesForRun` — sub-agents silently kept the static baked body forever. Now matches the three top-level run-creation sites.
  2. `resolveSkillBodiesForRun` aborted the whole agent on a single store error instead of falling back per-skill. Now `continue`s like the JSON-parse branch directly below.
- **Operator-trust ctx synthesis fix.** The first cut of the gRPC substrate handlers (PR #163 commits 4) passed the raw inbound ctx straight to the Connector. Result: every gRPC AgentDef/SkillDef call hit the in-process tool's default-deny scope gate. Tests passed because the mock bypassed the gate. Now a `substrateGRPCCtx` mirrors the HTTP `substrateAdminCtx` and MCP `operatorCtx` — synthetic identity `grpc-admin` distinguishes the transport in audit logs. Both admin-ctx helpers widened to grant all six policies (Memory + Channel + AgentDef + SkillDef + Evaluation + History) for symmetry with MCP, so the shared `dispatchSubstrate` helpers are safe to extend.
- **Regression test.** `TestGrpcSubstrate_OperatorCtxLetsRealToolThrough` wires a real `*builtin.SkillDef` (not the mock) and asserts the scope gate permits a `list` op. If the ctx synthesis ever regresses, the test fails — the kind of bug that's invisible under mock-based tests but breaks production.

**Tests:** ~25 new tests across Go (+10), TS (+7), Python (+5), plus 3 regression/refactor updates to existing tests. Full sweep clean.

**Release artifacts:**

- **Binary (goreleaser → Homebrew tap):** v0.8.23 tag — release.yml re-fired after the v0.8.23 initial-tag-attempt hit `git is in a dirty state` (Makefile's `npm install` mutated `web/package-lock.json` before goreleaser ran; fixed by switching to `npm ci`).
- **TypeScript adapter (`@loomcycle/client@0.8.23`):** published to npm on the v0.8.23 tag.
- **Python adapter (`loomcycle==0.7.0`):** **NOT** on PyPI as of v0.8.23. The PyPI Trusted Publisher needs configuration at pypi.org; until then the Python adapter ships from source via `pip install "git+https://github.com/denn-gubsky/loomcycle@python-v0.7.0#subdirectory=adapters/python"`. The `python-v0.7.0` tag still exists for source installs even though the PyPI publish failed. Operator action: configure Trusted Publisher at pypi.org/manage/account/publishing/ then re-run the failed `Publish loomcycle to PyPI` job from the Actions tab.

**Out of scope:** Exposing `Memory` / `Channel` / `Evaluation` / `Context` on HTTP/gRPC/adapters (they have MCP meta-tools today; same gap AgentDef had before this PR). Convenience wrappers (`forkSkillDef`, `promoteSkillDef`) on the adapters — single op-discriminated method per tool keeps the wire surface minimal.

## v0.8.22 — earlier

**Status: shipped (2026-05-20).** **`SkillDef` tool — runtime-mutable skill substrate.** Mirror of `AgentDef` (v0.8.5) but for SKILL bodies. Six PRs land the storage layer, the tool, policy plumbing, the API wiring, and snapshot completeness.

**What's in v0.8.22 (vs v0.8.21):**

- **Six new operations.** `SkillDef.{create, fork, get, list, retire, promote}` mirror `AgentDef`'s surface exactly. `create` refuses names that exist in the static `skills.Set` (operator's SKILL.md is ground truth); `fork` bootstraps v1 from the static body when no DB row exists yet (`bootstrapped_from_static=TRUE`). `promote` is explicit — selection stays policy, loomcycle does NOT auto-promote.
- **Storage parity with AgentDef.** Two new tables (`skill_defs` + `skill_def_active`) via Postgres migration 0016 + SQLite inline schema. Same `(name, version)` UNIQUE for monotonicity, same `parent_def_id` lineage chain, same per-name advisory lock (Postgres) / `BEGIN IMMEDIATE` (SQLite) for concurrent forks. Nine new Store CRUD methods + four snapshot methods. New `ErrSkillDefParentNotFound` typed error.
- **Overlay shape.** `{body, description, allowed_tools}` — three fields, not the AgentDef union. `body` is required on `create`/`fork` (empty / whitespace-only is rejected — zero-body skill is silent prompt corruption). `allowed_tools` ⊆ calling agent's effective `AllowedTools` (reuses `assertAllowedToolsSubset` from `agentdef.go` — same package).
- **Scope policy.** New `tools.SkillDefPolicyValue{Scopes}` ctx-attached at all four run-creation sites + the sub-agent dispatch. Closed grammar: `any` / `named:<skill-name>` / `descendants`. No `self` — skills have no agent identity, so the scope is meaningless. Yaml field `skill_def_scopes:` with `validateSkillDefScope` (config-load).
- **Runtime resolution — Approach A (system-prompt bake).** `Server.resolveSkillBodiesForRun` rebuilds the agent's effective `SystemPrompt` at session creation when any of the declared `skills:` has a DB-active row. Fast path: no DB rows → unchanged (the config-load baked prompt is correct). Slow path: `SystemPromptBase` (captured at config-load by the now-non-destructive `resolveSkills`) + per-skill (DB-active OR static) body. In-flight runs keep their locked prompt — no mid-run skill swap.
- **Runtime resolution — Approach B (`Skill` tool).** The `Skill` tool gains a `Store` field; `Execute` consults `SkillDefGetActive(name)` first, falls back to the static `Set` on miss. Same allowed-tools-subset security check on both paths.
- **Context tool integration.** `Context.permissions` op gains a `skill_def_scopes` field alongside the existing `agent_def_scopes`.
- **Snapshot completeness.** Envelope grows two new sections (`skill_defs` / `skill_def_active`) with identity migrators at `"1.0"`. `RestoreResult` gains `SkillDefsRestored` + `SkillDefActiveRestored` counters. Migration registry test asserts both new sections walk cleanly.
- **Internal RFC reversal.** The loomcycle-internal Hermes comparison RFC currently places runtime skill mutation in "Tier D — deliberately don't adopt." This work reverses that *as substrate only* — selection remains policy, no autonomous skill creation. The RFC entry moves Tier D → Tier A in a follow-up on the operator-side repo.

**Tests:** 8 new storetest contract tests (mirroring the AgentDef cases) + 10 SkillDef tool tests + 4 per-run resolver tests + extended Skill tool + Context permissions tests. All pass on SQLite; Postgres adapter is covered by the shared contract suite.

**Out of scope:** Hermes-style autonomous skill creation. Admin HTTP endpoint for SkillDef inspection (tool-only surface in this PR). `prefix:` scope for either AgentDef or SkillDef (kept symmetric). Live filesystem-watcher reload of the static `skills.Set`. Per-tenant `skill_def_active` pointer (same TODO posture as AgentDef). gRPC + MCP exposure of SkillDef (tool-only).

## v0.8.21 — earlier

**Status: shipped (2026-05-20).** **Activity Monitor tab + audit view + UI polish.** New `/ui/activity` page exposes the v0.8.11 process-resource sampler as live charts (memory vs running agents, CPU load, queue depth, plus diagnostic charts behind an `advanced` toggle). New `/ui/audit` page over a new `GET /v1/_events` admin endpoint (paginated cross-session event log, filterable by event type + date range). `/healthz` extended with `metrics_enabled` so the UI renders its "sampler off" empty state without probing `/v1/_metrics` first. Web UI polish — proper nav-tab underlines, dynamic version surfaced from the running binary (was hard-coded `v0.8.17`), copy buttons on every expanded EventCard, resizable panes on `runs` + `memory` via a new `<Splitter>` component, running-agent tree rows get tinted backgrounds matching their await-state chip colour (green = running, orange = waiting on `Channel.subscribe`, violet = waiting on `Interruption.ask`).

## v0.8.18 — earlier

**Status: shipped (2026-05-19).** **Cross-transport hardening of v0.8.17 Pause/Resume/Snapshot.** v0.8.17 shipped real implementations behind HTTP + CLI + Web UI but left the `connector.Connector` interface — the architectural seam every wire transport translates through — in its v0.8.15 PREVIEW state. MCP handlers dispatched correctly through the Connector but received mocked data; gRPC had no pause/snapshot RPCs at all; Python adapter had no methods. v0.8.18 fixes all three in three focused PRs. PRs #136, #137, #138.

**What's in v0.8.18 (vs v0.8.17):**

- **Real Connector impls (PR #136 — the keystone).** `internal/api/http/connector_impl.go` swaps the 8 mocked Pause/Snapshot method bodies to real delegation: `PauseRuntime` → `s.pauseMgr.Pause`, `CreateSnapshot` → `snapshot.Capture + Store.SnapshotCreate`, `RestoreSnapshot` → `snapshot.Restore` (with `resolver.ForceProbe` callback for matrix refresh), etc. Wire shapes byte-compatible with v0.8.15 — only the response semantics flip from placeholder to authoritative data. MCP becomes real for free (handlers already dispatch through Connector).
- **New `GetSnapshot` Connector method.** Additive — distinct from `ExportSnapshot` (operator-facing "where did this land on the host") and `GetSnapshot` returns the full envelope including JSON content. Brings MCP tool count 21 → 22.
- **Typed Connector errors.** New `internal/connector/errors.go`: `ErrPauseNotConfigured`, `ErrAlreadyPausing`, `ErrNotPaused`, `ErrSnapshotNotFound`, `ErrSnapshotTooLarge`, `ErrSnapshotVersionTooNew`, `ErrSnapshotVersionUnknown`. Every transport translates to protocol-specific status codes (HTTP 503/409/404/413/422; gRPC `Unavailable` / `FailedPrecondition` / `NotFound` / `ResourceExhausted` / `FailedPrecondition`; Python typed exception subclasses).
- **`ExportSnapshotResult.RawJSON []byte`** (additive) — canonical envelope bytes for transports that stream exports. `FilePath` / `Checksum` stay empty when bytes-only path is used.
- **MCP PREVIEW markers removed.** Tool descriptions in `internal/api/mcp/tools.go` rewritten to describe the real behavior. The mock-only `toolErrWith` helper (which returned `(result, isError=true)` simultaneously for the export/restore mock path) is deleted; real impls return `(result, nil)` on success or `toolErr(...)` on failure.
- **gRPC surface (PR #137 — the catch-up).** 9 new RPCs in `proto/loomcycle.proto`: `PauseRuntime`, `ResumeRuntime`, `GetRuntimeState`, `CreateSnapshot`, `ListSnapshots`, `GetSnapshot`, `ExportSnapshot`, `RestoreSnapshot`, `DeleteSnapshot`. Each handler dispatches `s.connector.X(ctx, ...)` + maps typed errors to gRPC status codes via `translatePauseSnapshotError`. 17 handler tests using a `pauseSnapshotMock` purpose-built stub.
- **Python adapter (PR #138).** 9 async methods on `LoomcycleClient` mirroring the gRPC RPCs. 6 new typed errors with smart `_raise_from_grpc` discrimination by message text (e.g., `NotFound` + "snapshot" → `SnapshotNotFoundError`; `Unavailable` + "pause not configured" → `PauseNotConfiguredError`, a subclass of `UnavailableError` for back-compat). Version bump v0.5.5 → v0.6.0 (additive). 20 new tests; existing 35 unchanged.
- **TypeScript adapter maturation (PR 5a/5b/5c).** v0.1.0-alpha.0 → v0.8.18. The single-file `runStreaming`-only adapter is split into a module foundation (`client.ts` / `types.ts` / `errors.ts` / `fetch-helpers.ts` / `stream.ts`); 23 new methods land on top covering run continuation, agent metadata, transcript, health, admin user list, pause/resume/state, snapshot lifecycle (capture / list / get / export / restore / delete), memory admin, and interruption resolve — 24 total public methods. 14 typed error classes mirror the Python adapter's `errors.py` taxonomy 1:1 with HTTP status + body-text dispatch via `raiseFromResponse` (single source of truth). Vitest test suite added: 61 cases covering method-level request/response round-trip, every status-code → typed-error mapping, and SSE parser boundary cases. `runStreaming()` behavior byte-identical from v0.1.0-alpha.0 (jobs-search-agent's existing usage continues to work without changes).

**Tests:** 16 new HTTP Connector impl tests + 17 new gRPC handler tests + 20 new Python tests (9 error mappings + 11 client integration). Existing test suites untouched. Total v0.8.18 PR cycle: ~53 new tests, all green.

**Architectural note:** the v0.8.15 `Connector` abstraction worked exactly as intended — it absorbed the v0.8.17 → v0.8.18 cycle without protocol breakage. The 8 PREVIEW wire shapes locked in v0.8.15 carried unchanged through v0.8.18; orchestrators built against the v0.8.15 contracts (mock or not) keep working. This is the architectural seam paying off.

For the v0.8.17 baseline that drove this work, see [v0.8.17](#v0817--earlier).

## v0.8.17 — earlier

**Status: shipped (2026-05-18).** **Pause / Resume / Snapshot** — the v0.8.x → v0.9.x bridge. Runtime-wide quiesce + cross-version-portable JSON snapshot. PRs #129, #132, #134 (5-PR plan, re-bundled into 3 squash merges after the stacked-PR base-targeting quirk: PR #129 = pause primitive; PR #132 = snapshot storage + capture + export/restore; PR #134 = pause/resume/state HTTP endpoints + CLI subcommands + Web UI).

**What's in v0.8.17 (vs v0.8.16):**

- **Pause / Resume HTTP endpoints (#134).** `POST /v1/_pause` body `{"timeout_ms"?: N}` declares the runtime is quiescing. Idempotent tools (`Read`, `WebFetch`, `WebSearch`, `Memory.get/list`, `Channel.peek`, `AgentDef.get/list`, `Context.*`, `Evaluation.get/aggregate`) are cancelled IMMEDIATELY — their ctx flips to Done at pause-declare time. Non-idempotent + external (MCP) tools get a 30 s default grace window (operator-configurable via `LOOMCYCLE_PAUSE_DEFAULT_TIMEOUT_MS`, clamped at 5 min) then force-cancel. New `/v1/runs` requests get 503 while in `pausing` or `paused`. `POST /v1/_resume` flips back to `running` and re-marks each previously-paused run; the runner goroutines pick up the broadcast channel and re-enter their loops. `GET /v1/_state` returns `{state, paused_runs_count}` for dashboards.
- **Pause manager (#129).** New `internal/pause/` package: atomic state + broadcast channel + in-flight tool registry. `Manager.ToolCtx` is the iteration-boundary hook that wraps every tool dispatch with the per-category cancel policy (`CategoryIdempotent` cancels immediately; `CategoryNonIdempotent`/`CategoryExternal` get the deadline). Race-clean post-drain sweep in `finalizePause` ensures `activeTools` is empty when state transitions to `StatePaused` — operators relying on `PausedRunsCount` as a quiescence signal need this guarantee.
- **Per-run `pause_state` column on `runs` (#129).** Additive migration `0012_runs_pause_state.up.sql` (SQLite + Postgres). Partial index `runs_by_pause_state` keeps the resume sweep cheap. Three values: `running` (default), `pausing` (loop winding down to boundary), `paused` (loop hit the boundary, awaiting resume).
- **Snapshot storage + capture (#132).** New `snapshots` table (additive migration `0013_snapshots.up.sql`; SQLite TEXT-as-JSON, Postgres JSONB). `internal/snapshot/` package: `Capture(ctx, store, opts)` reads seven sections (`agent_defs`, `agent_def_active`, `memory`, `channels`, `evaluations`, `paused_runs`, optional `interaction_history`) into a JSON envelope with per-section semver. `DefaultMaxBytes = 512 MB`. `ID` format `snap_<unix_ms>_<8hex>` — sortable by capture time. Six new Store bulk-reader methods (`SnapshotReadAgentDefs`, `SnapshotReadMemory`, etc.); existing tools (Memory, Channel, Evaluation) unchanged.
- **Per-run transcript reads are best-effort.** Capture's `capturePausedRuns` continues on a single bad transcript — the failing run's entry gets a `transcript_error` field set; capture proceeds with remaining runs + sections. The RFC describes capture as best-effort representing store state at the read instant; losing every other section to one bad transcript would contradict that.
- **Restore + per-section migration registry (#132).** `Restore(ctx, store, raw, opts)` decodes via a deferred `map[string]json.RawMessage` so each section's bytes pass through `migrations.Migrate()` before typed decode. Nine new Store methods (`SnapshotRestoreSession` / `Run` / `Event` / `AgentDef` / `AgentDefActive` / `Memory` / `ChannelMessage` / `ChannelCursor` / `Evaluation`) all return `(inserted bool, err error)` using `rows_affected` from the driver. Idempotent: `ON CONFLICT DO NOTHING` (Postgres) / `INSERT OR IGNORE` (SQLite). Restore counters reflect `rows_actually_written`, not `rows_attempted` — a re-restore reports 0 even though every per-row INSERT "succeeded".
- **Session FK synthesis.** `paused_runs` reference `session_id` but sessions aren't a captured section. Restore synthesizes `snap_sess_<run_id>` deterministically before inserting the run; the `synthesized_sessions` counter on the response surfaces this so operators can audit cross-instance restore behaviour.
- **Resolver `ForceProbe` callback (#132).** Restore triggers an immediate resolver-matrix refresh before returning so operators can call `/v1/_resume` right after restore without waiting for the periodic probe. Implemented via `Resolver.SetForceProbeCallback(fn)` wired from `main.go` after the probe loop starts.
- **Numeric per-component version compare.** The per-section migration registry compares versions numerically (`"1.10" > "1.9"`) rather than lexicographically — a reader at section-version "1.10" no longer wrongly rejects a valid older snapshot at "1.9" as "too new" once a section bumps past minor 10.
- **Export + restore HTTP endpoints (#132).** `GET /v1/_snapshots/{id}/export` streams the raw JSON envelope with `Content-Disposition: attachment; filename="<id>.json"` (snapshot id sanitized against header injection). `POST /v1/_snapshots/{id}/restore` body `{"include_history?": false, "json?": <envelope>}` — the inline-JSON path bypasses the id lookup for CLI / cross-instance restore.
- **CLI subcommands (#134).** Seven new verbs: `loomcycle pause [--timeout-ms N]`, `resume`, `state`, `snapshot [--description S] [--include-history --since RFC3339]`, `snapshots list [--limit N] [--label-contains S]`, `snapshots export <id> [--out file.json]`, `snapshots delete <id>`, `restore <file.json>`. All share `--target` / `--token` flags defaulting to `LOOMCYCLE_BASE_URL` / `LOOMCYCLE_AUTH_TOKEN`. Exit codes: `0` success, `1` operational (5xx, network, 409 runtime-state), `2` user error (bad flag, 4xx other than 409). 409 → exit 1 matches scripts using `set -e` around idempotent pause loops.
- **Web UI (#134).** New `<PauseControls>` topbar component: pill showing current state (`running` / `pausing` / `paused`) with paused-runs overlay; Pause / Resume button confirms before action. Polls `/v1/_state` every 5 s; renders nothing on 503 so dev setups without a pause manager stay clean. New `/ui/snapshots` admin page: capture / restore-from-file / export-as-download / delete. Restore confirm flow shows per-section counters + warnings.
- **Context tool `history` op `since_ts` addendum (#132).** The v0.8.7 history op now accepts a `since_ts` RFC3339 filter. Pairs with the snapshot's interaction_history section: restored experiments call `Context.history(since=<experiment_start_ts>)` to reflect on past conversations after cross-version migration.
- **State-change signals flow through `_system/runtime-state`** (v0.8.6 system channels, shipped). No new SSE event types — operator dashboards and external orchestrators subscribe to the existing channel.

**Tests:** 8 HTTP handler tests (pause/resume/state — happy paths, 409 conflict, body-timeout override, invalid JSON, state-with-paused-rows). 16 CLI tests (httptest stub servers — exit-code mapping including 409 → exit 1, bearer-token plumbing, file I/O for export/restore, missing-arg user-error paths). 18 store contract sub-tests for SnapshotCreate/Get/List/Delete across SQLite + Postgres. 9 snapshot.Capture() unit tests + 14 Restore round-trip tests (idempotent, version-rejection, session-FK synthesis). 15 pause manager tests including the post-drain sweep + late-Store race regression.

**Sharp edges / forward-compat notes:**

- Memory section's schema reserves an optional `embedding` field (always null in v0.8.17; populated by v0.9.x semantic memory) so a snapshot captured today round-trips cleanly through a future loomcycle that does have vector ops — no v1.0 → v1.1 schema migration of the just-shipped data.
- Snapshot scope is running-state only. External DB backups handle archival history. The optional `include_history` flag adds an interaction-history section but is not how you back up a busy production database.
- Pause is runtime-wide. Per-tenant fairness defers to v0.9.x.
- Not encrypted at rest. Operator's disk-encryption policy applies — same posture as transcripts and Memory.

For the v0.8.16 baseline that drove this work, see [v0.8.16](#v0816--earlier). For RFC-level design, see `~/work/loomcycle-internal/doc-internal/rfcs/pause-resume-snapshot.md`.

## v0.8.16 — earlier

**Status: shipped (2026-05-16).** **Interruption tool** — human-in-the-loop primitive. The v0.8.x arc complete (Memory + Channel + AgentDef/Eval + Context + LoomCycle MCP + Interruption = full substrate). PRs #119–#123.

**What's in v0.8.16 (vs v0.8.15):**

- **Three ops on a closed-enum `kind` discriminator**: `ask(question, options?, context?, timeout_ms?, priority?)`, `notify(message, priority?)`, `cancel(interrupt_id)`. The `kind` enum is `question` today; `pause` / `wait_until` / `approval` reserved for future additive enum values so the storage schema and channel namespace stay stable.
- **Three delivery backends**: built-in Web UI (`/ui/interrupts`, default for production), consumer-side MCP (`mcp_server:<name>` — agent reaches the consumer's MCP server like any other), CLI (local dev reads answers from stdin). Operator picks via `interruption.backend` in yaml.
- **Storage**: new `interrupts` table (additive migration `0011`, SQLite + Postgres) + 8 Store methods. `id` format `intr_<unix_ms>_<8hex>` sortable. Status discriminator (`pending` / `answered` / `cancelled` / `expired`); periodic sweeper marks expired rows.
- **Signal flow via `_system/interrupts/*` channels** (v0.8.6 system channels — shipped). `interrupts/pending` (scope=`user`, per-user isolation via Channel ACL) + `interrupts/answered`. The Web UI subscribes via the existing Channel SSE; consumer-MCP and CLI backends consume the same channels. **No new SSE event types** — channels carry the signal.
- **bus.Wait blocking with sub-ms wake.** The tool's `ask` op blocks the agent loop on a `bus.Wait(interrupt_id, timeout)` — the v0.8.4 Channel bus pattern. On resolve, the answer endpoint publishes to the bus + the channel; the blocked agent wakes within sub-ms.
- **LoomCycle MCP gains a 21st meta-tool**: `interruption_resolve` (#121) so Claude Code or any external MCP orchestrator can be the answerer — completing the consumer-MCP backend trio.
- **Per-agent ACL**: `interruption: {enabled, kinds, max_pending}` yaml. Default-deny (parallel to `memory_scopes` / `channels`). The run's `user_id` is the authoritative answerer.

For the v0.8.15 baseline that drove this work, see [v0.8.15](#v0815--earlier). For RFC, see `~/work/loomcycle-internal/doc-internal/rfcs/interruption-tool.md`.

## v0.8.15 — earlier

**Status: shipped (2026-05-14).** **LoomCycle MCP server + `Connector` abstraction.** The v0.8.x **capstone**: loomcycle exposes itself as an **MCP server** over stdio (Claude Code first; HTTP-MCP transport deferred to v0.8.15.x). New `connector.Connector` Go interface unifies HTTP, gRPC, MCP, and future CLI surfaces around a single contract — HTTP server is the canonical implementation, others CONSUME via direct method dispatch. PRs #99, #100.

**What's in v0.8.15 (vs v0.8.14):**

- **`connector.Connector` interface (#99) — the architectural anchor.** New `internal/connector/` package: 20-method Go interface that every wire transport translates into. `*lchttp.Server` IMPLEMENTS it (530 LOC of method implementations in `connector_impl.go`); `*lcmcp.Server` + `*loomgrpc.Server` CONSUME it via direct method dispatch (no HTTP round-trips). Compile-time assertion `var _ connector.Connector = (*Server)(nil)` prevents drift. TS/Python adapters mirror the same operation surface in their own languages over the HTTP wire. Interface stays ADDITIVE through v0.8.x — adding methods is safe; signature changes are a v0.9.x semver break.
- **MCP server (#99) — 20-tool surface.** New `internal/api/mcp/` package: stdio JSON-RPC loop + `initialize`/`tools/list`/`tools/call` handlers + per-session capability tracking + 20 tool handlers covering run lifecycle (spawn_run, cancel_run, get_run, list_runs), agent management (register_agent, unregister_agent, list_agents), all 5 v0.8.x builtins (memory, channel, agentdef, evaluation, context), and PREVIEW-mocked Pause/Resume/Snapshot (pause_runtime, resume_runtime, get_runtime_state, create_snapshot, list_snapshots, export_snapshot, restore_snapshot, delete_snapshot). Frame dispatch is sequential per MCP's initialization contract; concurrent tools/call within a connection is a v0.9.x optimization.
- **Streaming via MCP notifications (#99).** When the client opts in via `initialize.capabilities.loomcycle.runEvents=true`, `spawn_run` drives `runner.RunOnce` directly and emits `notifications/loomcycle/run_event` per provider event before returning the final result. Wire-ordering invariant pinned: every notification lands on stdout BEFORE the response. Without opt-in, blocking-only path via `Connector.SpawnRun`. Both paths produce identical final `SpawnRunResult` shape.
- **Dynamic agent registration (#99).** New `dynamic_agents` table (SQLite + Postgres migration 0010) + 5 Store methods + TTL sweeper. `mcp__loomcycle__register_agent` persists agents at runtime; `dynamic_agents_by_expires_at` partial index drives the periodic sweep. Privileged tools (Bash/Write/Edit) stripped from `allowed_tools` unless `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1` (default-deny per v0.8.7/v0.8.8 pattern). Name collisions with static yaml agents rejected.
- **`loomcycle mcp --config Y` subcommand (#99).** New entry point that starts BOTH the HTTP listener AND the stdio MCP loop. Logs to stderr (stdout is the JSON-RPC wire). Companion `loomcycle-mcp.sh` wrapper at the repo root sources `.env.local` before exec — required because Claude Code's MCP spawn inherits an empty env, missing the `LOOMCYCLE_*` + provider keys that upstream MCP server `${...}` placeholders expect. Without the wrapper, upstream handshakes fail and stdio readiness blocks for ~32s of exponential backoff.
- **gRPC server dispatches through Connector (#99).** `internal/api/grpc/server.go` now holds BOTH a `connector.Connector` field (used by `CancelAgent` and future proto handlers) AND the existing `runner.Runner` field (used by streaming `Run` / `Continue` — `Connector.SpawnRun` is blocking-only). Existing tests pass with `Connector=nil` (legacy direct path retained for backwards compat). New regression: `TestGrpcServer_CancelAgent_DispatchesThroughConnector`.
- **Operator ctx for builtin wrappers (#99 review fix).** Caught during code review: bare-ctx dispatch to `tool.Execute` left every builtin wrapper failing with "no scope configured" because policy values weren't on ctx. New `internal/api/mcp/context.go operatorCtx()` enriches ctx with all 5 policy values (memory/channel/agentdef/evaluation/history) + synthetic `RunIdentity{UserID: "mcp-operator", AgentID: "a_mcp-operator"}` + `AgentName: "mcp-operator"` before each builtin wrapper invocation. Pinned by `TestOperatorCtx_AttachesAllRequiredPolicies` — future tools growing new policy gates force an update here.
- **`spawn_run` schema fix (#99 review fix).** Original schema declared `["agent", "segments"]` required, blocking session_id-only continuations from schema-validating MCP clients (Claude Code). Replaced with `anyOf: [{required: agent}, {required: session_id}]`. `allowed_hosts` doc gap also closed (omit = no narrowing; `[]` = deny-all).
- **Malformed-frame -32700 path (#99 review fix).** Bad probe-unmarshal previously silently swallowed the frame, stalling the MCP client forever waiting for a response. Now emits a best-effort `-32700` with `id=0` so the client gets some recovery signal.
- **3 new env vars** documented in `.env.example` (#100): `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS` (default 0), `LOOMCYCLE_DYNAMIC_AGENT_DEFAULT_TTL_SECONDS` (default 86400), `LOOMCYCLE_DYNAMIC_AGENT_SWEEP_INTERVAL_MS` (default 900000).
- **doc-internal migration (#100).** Internal design docs (PLAN.md, RFCs, decision history) moved from `doc-internal/` inside this repo to `~/work/loomcycle-internal/doc-internal/` — a separate operator-side repo. The in-repo folder (always gitignored) was deleted; `.gitignore` + `CLAUDE.md` references updated in lockstep.

**11 MCP server unit tests** cover handshake, tools/list (20 tools), spawn_run blocking + streaming, notification-before-response ordering, register_agent dispatch, unknown tool → -32601, malformed frame → -32700, sequential dispatch (5 requests), pause_runtime preview shape, operatorCtx policy contract. Plus 1 gRPC dispatch-through-connector regression test. `go test -race ./...` clean across all 41 packages.

**Sharp edges (deferred to v0.8.16):** boot-time upstream MCP init can block stdio readiness for ~32s if an upstream is misconfigured (wrapper mitigates); HTTP listener binds 127.0.0.1:8787 alongside MCP (operators can't run `loomcycle.sh` daemon AND `loomcycle mcp` simultaneously); Pause/Resume/Snapshot ship as PREVIEW shapes (real implementation in v0.8.16+, wire is stable).

**Operator integration recipe** — project-root `.mcp.json`:
```json
{"mcpServers": {"loomcycle": {"command": "/abs/path/to/loomcycle/loomcycle-mcp.sh"}}}
```
Or via `claude mcp add loomcycle /path/to/loomcycle-mcp.sh` (writes to `~/.claude.json`). **Note:** `~/.claude/mcp.json` is NOT a discovered location.

For the v0.8.14 baseline that drove this work, see [v0.8.14](#v0814--earlier).

## v0.8.14 — earlier

**Status: shipped (2026-05-13).** **Per-run MCP bearer tokens + auto-version + metrics CI fix.** Three landings bundled: per-end-user auth for MCP tool calls (the marquee), build-identity self-reporting via Go's VCS stamp, and the metrics sampler bugfix that had been silently red-CI since v0.8.11. PRs #94, #95, #96.

**What's in v0.8.14 (vs v0.8.13):**

- **Per-run MCP bearer tokens (#94).** Operator yaml `mcp_servers.*.headers` can now reference `${run.user_bearer}` and `${run.user_bearer:-FALLBACK}` (POSIX-style default). The HTTP MCP transport substitutes at outbound request-build time inside `Client.do()` from a ctx-carried bearer (`tools.RunIdentityValue.UserBearer`); pool construction is unchanged so the `Client` stays shared across runs without per-run instantiation. New `user_bearer` wire field on `runRequest` + `messagesRequest` (per-request, not session-bound — continuations may rotate), charset `[A-Za-z0-9._\-+/=]{16,512}` → 400 otherwise. Sub-agents inherit the bearer identically (NOT narrowed — they act on behalf of the same end-user). Missing bearer with no fallback drops the header + WARN logs (downstream MCP returns clean 401, more debuggable than a loomcycle-side dispatch error). Nested `${run.user_bearer:-${LOOMCYCLE_STATIC_BEARER}}` composes by design: the existing `expandEnv` regex structurally cannot match `${run.*}` tokens (the `.` fails its `[A-Za-z0-9_]*` char class), so the inner `${LOOMCYCLE_*}` resolves at yaml-load and the outer survives to request-time. **Motivation:** JSA's `/api/mcp` route authenticates per-end-user via Bearer tokens; before v0.8.14, every run authenticated as the operator (`denn`), not the actual end-user — closing the per-tenant auth gap was the whole point. 10 validation cases + 7 substitute-helper unit tests + 3 client integration tests (incl. concurrent-run isolation regression guard) + sub-agent inheritance test + 2 expandEnv namespace regression guards.
- **Auto-version from `runtime/debug` (#95).** `--version` now reports `version=<v> commit=<c> built=<t> go=<g>` derived automatically from Go's embedded VCS stamp (every binary built with `go build` / `go install` since Go 1.18). Release scripts can still override via `-X main.buildVersion=...` ldflags. Boot-log line carries the same identifiers so an operator running stale code spots it before any "but my code says X" spiral. Module-version path surfaces a clean `v0.8.14` for tagged-commit builds and a pseudo-version (`v0.8.14-0.YYYYMMDD-HASH`) for commits past a tag; dirty working trees append `-dirty`. **Motivation:** the v0.8.13 deploy used `-X main.gitCommit=...` (wrong variable name — the var is `buildCommit`) and silently shipped `commit=unknown` to the VM. Unattached versioning makes "is this the binary I just built?" answerable without release-script ceremony. 7 format cases + smoke test.
- **Metrics sampler /proc-counter fix + gofmt drift (#96).** CI on every main commit since v0.8.11 had been failing on `TestSampler_GracefulStoreError` and `TestSampler_RecoveryResetsCounter`. Root cause: GitHub Actions Ubuntu runners' `/proc/self/status` lacks the `VmRSS:` line; the sampler conflated `/proc`-read errors with store-write errors in a single `s.failures` counter. One tick on CI produced `failures=2` (proc inc + write inc) and `logs=2` instead of the expected 1/1. Fix: new `Sampler.procReadFailureLogged bool` — proc errors log once per program lifetime via `log.Printf` (decoupled from `cfg.Logf` which tests use as the store-write counter); `s.failures` now means exclusively "consecutive store-write failures" matching the documented intent. Plus 5 gofmt-drift files cleaned up to unblock the CI gofmt check (`internal/api/http/metrics_handlers.go`, `internal/api/http/server.go`, `internal/loop/fallback_test.go`, `internal/metrics/proc_linux.go`, `internal/providers/gemini/driver.go` — pure whitespace / import-order / struct-tag-alignment drift from prior PRs that landed before the CI gofmt step was added).

For the v0.8.13 baseline that drove this work, see [v0.8.13](#v0813--earlier).

## v0.8.13 — earlier

**Status: shipped (2026-05-13).** **Pin provider after first successful turn (opt-in).** Closes the entire class of cross-provider mid-conversation transcript bugs by suppressing provider fallback after a run has completed at least one successful turn. PR #93.

**What's in v0.8.13 (vs v0.8.12):**

- **New `FallbackPolicy.PinAfterSuccess bool` field** — when true, `tryProviderFallback` in `internal/loop/loop.go` suppresses fallback if any turn has succeeded (assistant message appended to conversation history). Initial-turn fallback still works (stale-probe safety net at run start); same-provider rate-limit retry continues to handle transient errors. Mid-conversation provider switches stop happening — the source of every DeepSeek 400 / Anthropic `cache_control` loss / Gemini `thoughtSignature` mismatch we'd discovered.
- **New typed `EventFallbackSuppressed` event** — fires whenever the pin policy intercepts a would-be fallback. Wire-stable for adapters; mirrors the v0.8.2 `EventCacheInvalidated` / v0.8.12 `EventReasoningInvalidated` event-on-policy pattern. `Text` field carries the cause error so operators can attribute the failure.
- **New env var `LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS`** — default OFF in v0.8.x (opt-in); default-on planned for v0.9.x once production-validated. Wired from `cfg.Env` through to `FallbackPolicy` via the HTTP server's `fallbackForRun`.
- **3 new regression tests** in `internal/loop/fallback_test.go`: headline (turn 2 503 is suppressed when flag on), initial-pick resilience (turn 0 fallback still works), and the flag-off-preserves-v0.8.2-behavior guard.
- **v0.8.12 `Message.Reasoning` strip kept as belt-and-suspenders** — when a deployment opts back into mid-session fallback later, the strip still works.

The trade: a sustained mid-conversation provider outage now fails the run instead of cascading. That's acceptable for most production deployments — provider outages are rare, clients retry, and mid-conversation translation bugs are subtle and silent (much worse failure mode).

For the v0.8.12 baseline that drove this work, see [v0.8.12](#v0812--earlier).

## v0.8.12 — earlier

**Status: shipped (2026-05-13).** **Strip `reasoning_content` on cross-provider fallback.** Fixes a production bug where mid-conversation provider fallback would 400 on the new provider because the conversation history carried thinking/reasoning content from the previous provider. PR #91.

**What's in v0.8.12 (vs v0.8.11):**

- **The bug**: a cv-batch-adapter run on `user_tier=high` routed to `gemini-2.5-flash`, completed turn 1 (with a tool call), then turn 2's call to gemini returned `503` ("This model is currently experiencing high demand"). Runtime fallback fired → `deepseek-v4-flash`. DeepSeek 400'd with `"The reasoning_content in the thinking mode must be passed back to the API."` The run died unrecoverably.
- **Root cause**: `Message.Reasoning` (single string field on `providers.Message`) carries no provider provenance. The OpenAI driver — which also backs DeepSeek (DeepSeek implements OpenAI-compatible chat completions) — unconditionally echoes `Reasoning` back as `reasoning_content` in the assistant turn on the wire. DeepSeek's API verifies that any echoed `reasoning_content` matches what IT produced for that turn and 400s if it can't verify. Cross-provider echoes always fail this check.
- **Fix**: when `tryProviderFallback` successfully switches providers, walk the in-flight `messages` slice and zero `Message.Reasoning` on every assistant turn. The new provider gets a clean history. Approach C1 from the architect plan; C3 layered on top.
- **New typed event `EventReasoningInvalidated`** — emitted when the strip pass cleared one or more assistant turns. Mirrors the v0.8.2 `EventCacheInvalidated` precedent (Anthropic cache_control on Anthropic→other fallback). Wire-stable for adapters; cost retros should treat the run's downstream iterations as reasoning-cold on the new provider. The `Text` field carries a human-readable summary (`"cleared reasoning_content from N assistant turn(s) on switch from <old> to <new>"`).
- **Safe across all current providers**: Anthropic uses typed content blocks for `extended_thinking`, not the Reasoning string field → immune. Gemini's driver doesn't write Reasoning today → strip is a no-op unless populated via PriorMessages from a continuation. OpenAI o-series tolerates missing `reasoning_content` (treats as no prior thinking). DeepSeek/o-series within their own family continue to round-trip correctly — the strip only fires on cross-family fallback. Tool calls in the same turn are unaffected (strip only touches the Reasoning string field, not `Content`).
- **3 regression tests** in `internal/loop/fallback_test.go`: `TestFallback_ReasoningStrippedOnProviderSwitch` (headline regression — verified to fail on pre-fix code with the EXACT production failure mode), `TestFallback_NoReasoningStrip_NothingToStrip` (guards against spurious event emission), `TestFallback_PartialStreamReasoning_NeverReachesMessages` (pins the drain-and-continue invariant for in-stream errors).

For the v0.8.11 baseline that drove this work, see [v0.8.11](#v0811--earlier).

## v0.8.11 — earlier

**Status: shipped (2026-05-13).** **Process-resource metrics sampler + `/v1/_metrics/*` API.** Built-in periodic CPU + memory sampler that runs as a background goroutine inside loomcycle, captures process RSS + Go heap + goroutine count + CPU% (and optionally system-wide CPU/mem) while at least one agent run is active, persists samples to a new `process_samples` table, and exposes them via three bearer-authed HTTP endpoints. Idle-gate on the concurrency semaphore — zero DB writes and zero `/proc` reads when nothing is running. Closes the capacity-planning gap that previously forced operators to sample `ps`/`top` via external shell scripts. PR #89.

**What's in v0.8.11 (vs v0.8.10):**

- **`internal/metrics/` package** — sampler core + Linux `/proc/self/status` (VmRSS) + `/proc/self/stat` (utime+stime delta CPU%) readers, optional `/proc/stat` + `/proc/meminfo` for system-wide. Build-tag-split (`proc_linux.go` / `proc_other.go`) so the binary compiles cleanly on macOS/Windows with the Linux-only fields landing as zero. Consecutive-failure rate-limited logging (loud on first error, every 10th thereafter) protects against log flood on a wedged disk.
- **Three bearer-authed endpoints** under `/v1/_metrics/*`:
  - `GET /v1/_metrics/samples?since=&until=&limit=&cursor=` — windowed raw samples with cursor pagination
  - `GET /v1/_metrics/runs/{run_id}` — peak/mean RSS + max CPU% for samples overlapping the run's `[started_at, COALESCE(completed_at, now)]` window. Computed via SQL JOIN — no denormalized columns on the `runs` table; in-flight runs return their elapsed-window samples.
  - `GET /v1/_metrics/summary?period=1h|24h|7d` — aggregated buckets (mean/max RSS, p95 CPU%, max active_runs per bucket).
- **5 new env vars**: `LOOMCYCLE_METRICS_ENABLED` (default OFF in v0.8.x, default-on planned for v0.9.x), `LOOMCYCLE_METRICS_SAMPLE_INTERVAL_MS` (default 5000; floor 1s to prevent write-storms), `LOOMCYCLE_METRICS_RETENTION_DAYS` (default 7), `LOOMCYCLE_METRICS_COLLECT_SYSTEM` (default OFF; Linux-only), `LOOMCYCLE_METRICS_SWEEP_INTERVAL_MS` (default 15 min).
- **Storage**: new `process_samples` table on both sqlite (CREATE TABLE in `stmts`; index in `addIndexes` per the v0.8.6 defensive habit) + postgres (migration `0009_process_samples`). `MintSampleID` helper mirrors `MintChannelMessageID`.
- **`cancel.Registry.ListAll()`** — general-purpose accessor for future cross-cutting consumers. Not used by the sampler in v0.8.x; shipped as a forward-compat addition with its own test coverage.
- **28 new tests**: 6 contract (auto-run on sqlite + postgres), 5 sampler unit, 8 `/proc` parser unit, 9 HTTP handler, 2 cancel registry. Race-detector clean.
- **Production-validated**: deployed to the operator's TrueNAS VM 2026-05-13; first run (employer-profiler + company-researcher sub-agent + 2 injection-judge sub-agents) captured 31 samples and revealed loomcycle's per-process footprint as 21–33 MB RSS across the entire 3-way concurrent run tree.

For the v0.8.10 baseline that drove this work, see [v0.8.10](#v0810--earlier).

## v0.8.10 — earlier

**Status: shipped (2026-05-13).** **Gemini schema sanitizer (`$ref` + combinators) + sqlite migration ordering fix.** Two operational fixes consolidated into one release.

**What's in v0.8.10:**

- **Gemini schema sanitizer rewritten** (`internal/providers/gemini/driver.go sanitizeGeminiSchema`). Production agents on Gemini were hitting `400 INVALID_ARGUMENT` whenever an MCP tool's input schema contained JSON-Schema features outside Gemini's OpenAPI-3.0 subset. The jobs-search-agent consumer's `zod-to-json-schema` conversion emits `$ref` + `oneOf` for every `z.discriminatedUnion` / shared sub-schema. The pre-existing sanitizer only stripped `additionalProperties` / `$schema` / `$id`. New behavior:
  - `$ref` inlining (cycle-safe via per-path visited-set; diamond refs each inline independently; cycles emit `{}`; unresolved refs emit `{}`)
  - `allOf` / `oneOf` / `anyOf` collapse by MERGING all variants' properties + required into the parent (an earlier first-variant-wins draft was caught in code review — it silently dropped every discriminated-union variant past the first, the exact regression the fix targeted)
  - Type-conflict defense (e.g. `oneOf[object, array]`) skips structural fields of the conflicting variant; folding them in would have produced a schema MORE broken than the input
  - 11 new sanitizer tests including a realistic Zod-shape MCP fixture
- **SQLite migration ordering fix** (`internal/store/sqlite/sqlite.go`). The v0.8.6 migration created `channel_messages_by_visible` index in the first `stmts` loop, BEFORE the `addColumns` ALTER block. On a fresh deploy this worked because `CREATE TABLE IF NOT EXISTS channel_messages (...visible_at...)` declared the column up front; on an UPGRADE from v0.8.4/v0.8.5 the existing table had no `visible_at` column and the CREATE INDEX failed with `SQL logic error: no such column: visible_at`. CI never caught this because every test run uses a fresh DB. Fix: moved the CREATE INDEX into the `addIndexes` block (which runs AFTER `addColumns`). Postgres is unaffected — migration `0005_channel_visible_at.up.sql` already orders ALTER → CREATE INDEX correctly inside a single golang-migrate transaction. Regression test `TestMigrate_UpgradeFromV084ChannelMessages` simulates the upgrade path and fails on the unfixed code with the exact production error.

PRs: #86 (Gemini schema), #87 (sqlite migration).

For the v0.8.9 baseline that drove this work, see [v0.8.9](#v089--earlier).

## v0.8.9 — earlier

**Status: shipped (2026-05-13).** **Gemini schema sanitizer (initial pass)** — same shape as v0.8.10's first change above. v0.8.9 shipped the schema sanitizer; v0.8.10 added the sqlite migration fix that became necessary when deploying v0.8.9 from a v0.8.4 schema. See v0.8.10 for the consolidated description.

For the v0.8.8 baseline, see [v0.8.8](#v088--earlier).

## v0.8.8 — earlier

**Status: shipped (2026-05-12).** **`Context.help`** — the tenth op on the v0.8.7 Context tool. With no `topic`, returns the topic index (`{topics: [{name, description, source}], count, hint}`); with `topic=<name>`, returns the full markdown body. Five topics ship bundled in the binary via `//go:embed` (`loomcycle`, `scopes`, `subagents`, `experimentation`, `system-channels`); operators override or extend with `LOOMCYCLE_HELP_ROOT`. Closes the gap between every tool's `doc` op (its own input schema) and cross-cutting guidance that no single tool's `doc` can return alone — how scopes compose across Memory + Channel, when sub-agents beat channels, what the AgentDef + Evaluation loop looks like end-to-end. PR #84.

**What's in v0.8.8 (vs v0.8.7):**

- **`help` op on the Context tool** with index-vs-detail split. Index mode keeps the response compact (no `content`); detail mode returns the full markdown body. Unknown topic surfaces the available list in the error so the model can self-correct in one round-trip.
- **Filesystem overlay via `LOOMCYCLE_HELP_ROOT`.** Files at the root with names matching bundled topics REPLACE them; new names extend the set. Same Claude-Code-compatible frontmatter (`name:` must match filename stem; `description:` is the index one-liner; everything after the closing `---` is the body).
- **Trust-boundary protections.** Symlinks under the help root are **refused** with a log line — a stray `escape.md` symlink would otherwise let an operator (intentionally or via misconfigured automation) exfiltrate any file the loomcycle process can read into the topic body the model sees. Per-file parse errors are **soft-skipped** rather than fatal so one malformed operator topic doesn't kill the runtime (bundled defaults remain intact).
- **Test coverage:** 16 unit tests in `internal/help/loader_test.go` (bundled load, filesystem overlay, override-by-name, symlink refusal, malformed soft-skip, frontmatter validation, subdir skip, nil-safety) + 4 unit tests for `execHelp` (nil refusal, index mode, detail mode, unknown topic). Race-detector clean. Runtime smoke at `test/runtime/context-help/` exercises both modes against `gemini-2.5-flash`.
- **`.env.example`** documents `LOOMCYCLE_HELP_ROOT` with frontmatter contract + override semantics.

For the v0.8.7 baseline that drove this work, see [v0.8.7](#v087--earlier).

## v0.8.7 — earlier

**Status: shipped (2026-05-12).** **Context tool** — read-only runtime introspection. Lets self-evolving agents inspect their own runtime: what tools they have, who they are, what their definition's lineage looks like, what evaluations exist for it, what channels they can reach, what their conversation history is. Nine ops on a single discriminated `op` field; auto-attached to every agent's `allowed_tools` at config-load.

**What's in v0.8.7 (vs v0.8.6):**

- **Nine ops, all read-only.** `self` (identity bundle from `RunIdentity` + `AgentName` ctx-keys), `tools` (post-filter tool catalog with closed-set side-effect classifier — `pure` / `state` / `network` / `filesystem` / `privileged` / `unknown`), `doc` (input schema + description for one tool by name; refuses outside the per-run allowlist — no doc leak), `permissions` (bundle of every policy ctx-key — `allowed_tools`, `host_policy`, `memory`, `channels`, `agent_def_scopes`, `evaluation_scopes`, `history_scope`), `agents` (operator-declared agents from `cfg.Agents` with active `def_id` from the v0.8.5 substrate; optional `prefix` filter), `lineage` (walks ancestors via `parent_def_id` chain + descendants BFS; `depth` default 10/cap 100; **total-node cap 500** with `truncated` flag), `evaluations` (v0.8.5 `EvaluationAggregate` output — mean/median/min/max/latest + per-dimension + per-emitter-role; optional `include_lineage` walks ancestors), `channels` (operator-declared channels with per-caller publish/subscribe bools; wildcards surface separately), `history` (transcript events for the target agent — default caller's own; optional `event_types[]` filter + `limit`; `truncated` is **honest under filter**).
- **Default-add behaviour.** Every agent's `allowed_tools` gets `Context` auto-appended at config-load. Opt-out is one yaml line: `disable_context: true`. The duplicate-check is **case-insensitive** so `[context, Context]` doesn't sneak through.
- **`history_scope` yaml gate (closed set, default-deny).** `self` (caller's own run — practical default), `siblings` / `descendants` / `named:<n>` (reserved — need `RunIdentityValue.ParentAgentID` plumbing), `any` (UNRESTRICTED — operator-trust grant for admin/debug agents).
- **PRs:** #79 (self/tools/doc/permissions), #80 (agents/lineage/evaluations), #82/#83 (channels/history + default-add + runtime smoke).

For the v0.8.6 baseline that drove this work, see [v0.8.6](#v086--earlier).

## v0.8.6 — earlier

**Status: shipped (2026-05-12).** **System channels + deferred publish.** Operator-declared `_system/*` channels published by loomcycle itself (cadence ticker for heartbeats; event hooks for state transitions) AND/OR by a bearer-authed admin endpoint. General `deliver_at` on `Channel.publish` for scheduled / deferred delivery. The Event vs Channel architecture review landed here: per-run transcripts stay as Events; cross-cutting role-(B) typed notifications (heartbeats, alarms, runtime-state, provider events) migrate to Channels. Future Question + Pause-state subsystems publish to `_system/questions/*` and `_system/runtime-state` rather than minting new typed SSE events.

**What's in v0.8.6 (vs v0.8.4):**

- **Three categories of `_system/*` channels** all in the standard yaml: (1) **Cadence** — `_system/heartbeat-1m`/`-5m`/`-1h` published by a `HeartbeatRunner` goroutine at fixed intervals; payload `{ts, version, uptime_s}`. (2) **Event-driven** — `_system/runtime-state`, `_system/provider-events` fire from internal subsystem hooks; no `period:` needed. (3) **Agent-publishable system channels** — `_system/alarms/critical`/`/warning`/`/info` reserved-by-convention; operators publish via the admin endpoint.
- **`SystemPublisher` interface + `StorePublisher` impl** — loomcycle-authoritative publish path that bypasses agent-tool ACL gates. Stamps `published_by_user_id="_system"` (internal) or `"_admin"` (admin-endpoint).
- **Tool-layer refusals.** Agents can NEVER publish to (a) `publisher: system` channels or (b) anything with the `_system/` prefix — defense-in-depth even when operator yaml is misconfigured.
- **Admin endpoint** `POST /v1/_channels/_system/{name…}` — bearer-authed, accepts `{payload, deliver_at?}` body.
- **Deferred publish (general).** `Channel.publish` accepts optional RFC3339 `deliver_at`; message stored immediately but hidden from reads until `visible_at`. In-process `time.AfterFunc(visible_at)` scheduler wakes long-poll subscribers exactly at delivery time; bounded by `LOOMCYCLE_CHANNELS_MAX_PENDING_DEFERRED` (default 10000). **TTL counts from `published_at`, NOT from `deliver_at`** — size your TTL to cover both deferral and visibility windows.
- **Tuple cursor `(visible_at, msg_id)`** replaces pure msg_id ordering — without it, subscribers would silently skip deferred messages once they progressed past the publish-time id. **Clean cursor break**: 0005 migration truncates `channel_cursors` (v0.8.4 only shipped two weeks earlier — no production cursor state worth preserving).
- **Audit column** `channel_messages.published_by_user_id` distinguishes operator / system / agent activity without grepping logs.
- **PRs:** #74 (deferred publish + tuple cursor), #75 (system publisher + admin endpoint), #76/#78 (heartbeat ticker + runtime smoke).

For the v0.8.5 baseline that drove this work, see [v0.8.5](#v085--earlier).

## v0.8.5 — earlier

**Status: shipped (2026-05-11).** **Self-Evolution Substrate** — three primitives shipped together: `AgentDef` tool (mutate definitions), versioned `agent_defs` storage with lineage, `Evaluation` tool (score (run, def) pairs). Closes the gaps that block the planned self-evolving agentic research program. Loomcycle deliberately does NOT auto-promote based on score — selection stays policy, lives in operator orchestrators or in agents calling `aggregate` + `promote` per their own strategy (max / GA / PPO / RLHF / whatever).

**What's in v0.8.5 (vs v0.8.4):**

- **`AgentDef` built-in tool — 6 ops.** `create` / `fork` / `get` / `list` / `promote` / `retire`. Static `cfg.Agents` names inviolate (must `fork`, never `create`). **AllowedTools is the operator's static capability ceiling** — forks may NARROW, never widen; enforced via 100-hop cycle-guarded lineage walks. Per-agent yaml `agent_def_scopes` gates `self` / `descendants` / `named:[...]` / `any`, default-deny.
- **`Evaluation` built-in tool — 5 ops.** `submit` / `get` / `list_for_run` / `list_for_def` / `aggregate`. Score model: required scalar + optional `dimensions` map + optional `judgement` JSON + optional `rationale` text. **`emitter_role` is derived server-side** from caller's `RunIdentity` vs target run's identity (`self` / `parent` / `external` / `unrelated`) — the model can't lie about who scored what. Known limitation: `sibling` collapses to `unrelated` (RunIdentityValue lacks emitter ParentAgentID); `submit_siblings` scope is reserved-but-inert.
- **Append-only `agent_defs` with monotonic version per name.** `parent_def_id` for lineage; `bootstrapped_from_static` flag for static-derived roots. `agent_def_active` pointer table for "which version a name resolves to." Promote/retire flip pointers — they never rewrite definition rows. Postgres `pg_advisory_xact_lock(hashtextextended('agent_def:' || name, 0))` serialises version allocation per name; sqlite uses pinned-conn + `BEGIN IMMEDIATE`. Tested under contention: 250 parallel forks → exactly versions 1..250 with no gaps or duplicates on both backends.
- **Sub-agent `def_id` pinning** via optional `def_id` on the `Agent` tool input. `runSubAgent` overlays the row onto static `cfg.Agents` for that one sub-run. `agent_def_id` persisted on the sub-run row + denormalised onto evaluations at submit — aggregate queries automatically partition by def. **Substrate policy fields stay with static yaml** (never in the overlay) so forks can't widen their own substrate-capability gates. **Cross-name pinning refused** — passing a `def_id` from a different agent name returns "cross-name pinning refused"; prevents namespace hijack.
- **Migrations (additive):** Postgres `0006_agent_defs`, `0007_runs_agent_def`, `0008_evaluations`. SQLite: idempotent `CREATE TABLE` + `ALTER TABLE runs ADD COLUMN agent_def_id TEXT`.
- **PRs:** #65 (storage + locks + aggregate kernel), #66 (config + ctx-key plumbing), #67 (AgentDef tool), #68 (Evaluation tool), #71 (runtime smoke), #72 (sub-agent def_id pinning).

For the v0.8.4 baseline that drove this work, see [v0.8.4](#v084--earlier).

## v0.8.4 — earlier

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

## v0.8.x — shipped (full timeline)

**Roadmap-renumbering postscript (updated 2026-05-18, fifth wave).** Sequenced 2026-05-09; renumbered through v0.8.5 as one-feature-per-point-release. The 2026-05-12 system-channels point release became v0.8.6; Context tool v0.8.7; Context.help v0.8.8. The 2026-05-13 production-readiness window absorbed six point releases: v0.8.9 + v0.8.10 (Gemini schema sanitizer + sqlite upgrade-path migration fix), v0.8.11 (process-resource metrics sampler), v0.8.12 (cross-provider reasoning_content strip), v0.8.13 (pin-after-success fallback policy), and v0.8.14 (per-run MCP bearer tokens + auto-version + metrics CI fix). The 2026-05-14 capstone window shipped v0.8.15 (LoomCycle MCP + Connector abstraction — the architectural anchor for every future wire transport). The 2026-05-16 Interruption wave shipped v0.8.16 (human-in-the-loop primitive). The 2026-05-18 bridge wave shipped v0.8.17 (Pause / Resume / Snapshot — the v0.8.x → v0.9.x bridge), completing the v0.8.x arc.

**v0.8.x shipped (full list):** v0.8.0 Memory tool (2026-05-09); v0.8.1 operational hardening (2026-05-10); v0.8.2 user_tier (2026-05-11); v0.8.3 ollama split (2026-05-11); v0.8.4 Channel tool (2026-05-11); v0.8.5 Self-Evolution Substrate (2026-05-11); v0.8.6 system channels + deferred publish (2026-05-12); v0.8.7 Context tool (2026-05-12); v0.8.8 Context.help (2026-05-12); v0.8.9 + v0.8.10 Gemini schema sanitizer + sqlite migration fix (2026-05-13); v0.8.11 process-resource metrics sampler (2026-05-13); v0.8.12 cross-provider reasoning_content strip (2026-05-13); v0.8.13 pin-after-success fallback policy (2026-05-13); v0.8.14 per-run MCP bearer tokens + auto-version + metrics CI fix (2026-05-13); v0.8.15 LoomCycle MCP server + Connector abstraction (2026-05-14); v0.8.16 Interruption tool (2026-05-16); v0.8.17 Pause / Resume / Snapshot (2026-05-18) — see the sections above for full release notes.

**v0.8.x arc complete.** The substrate primitives that compose into the agentic-OS kernel — Memory (state), Channel (IPC), AgentDef + Evaluation (self-mutation + selection), Context (introspection), Interruption (human bridge), LoomCycle MCP (control plane), Pause / Resume / Snapshot (runtime quiesce) — all shipped. v0.9.x picks up at the high-load runtime sweep + Semantic Memory (vector retrieval, RFC locked 2026-05-18).

## v0.9.x — high-load runtime sweep + Semantic Memory

Cross-cutting capacity items + one substrate feature (Semantic Memory). Not a single feature; collectively they take the runtime from "single-tenant comfortable on a 4–8 GiB VPS" to "10k concurrent agents per replica" while extending the v0.8.0 Memory tool with vector retrieval. Sequenced into v0.9.x as a series of small focused PRs.

- **Semantic Memory (vector retrieval).** RFC locked 2026-05-18 (`~/work/loomcycle-internal/doc-internal/rfcs/semantic-memory.md`). Extends the v0.8.0 Memory tool with a `search` op + an `embed: true` field on `set`; new `memory_embeddings` table joined on `(scope, scope_id, key)`. Backends: pgvector (Postgres) + sqlite-vec (SQLite), gated on operator env opt-in (`LOOMCYCLE_PGVECTOR_ENABLED=1`, `LOOMCYCLE_SQLITE_VEC_PATH=<.so>`). Server-side embedding via a new `providers.Embedder` interface (Anthropic + OpenAI + Gemini drivers). Quota math excludes embedding bytes. **Forward-compat:** the v0.8.17 Snapshot Memory section schema already reserves the optional `embedding` field (always null today), so snapshots captured before v0.9.x round-trip cleanly into a future loomcycle that has vector ops — no v1.0 → v1.1 schema migration of the just-shipped data.
- **Per-tenant fairness** in the concurrency layer. Currently every caller competes for one global semaphore — a noisy tenant monopolises the pool. Token bucket per `user_id` (or per `tenant_id` once that lands), with a small unfair share for global priorities.
- **In-memory run-status cache.** Today every `GET /v1/agents/{agent_id}` hits SQLite/Postgres. At 10k concurrent runs this is a hot path. LRU keyed on `agent_id` with sub-second TTL.
- **OpenTelemetry traces + Prometheus metrics endpoint.** Currently logs only. Per-run trace, per-tool-call span (the v0.7.1 hook seam is the wiring point), request rate / queue depth / semaphore-occupancy / provider-RTT histograms.
- **Heartbeat sweeper.** `last_heartbeat_at` is updated by the loop on every iteration but nothing reads it. A sweeper detects crashed runs (no heartbeat for > N minutes) and marks them failed so they don't stay `running` forever. (Schema-side already in place since v0.5.0.)
- **Session-lock map GC.** The HTTP server's `sync.Map` of session locks never garbage-collects entries (~32 B per session). Periodic sweep + bounded total entries.
- **Multi-replica HA.** Postgres for transcripts (already shipped in v0.5.0); Redis for in-flight cancel registry replication. Out-of-process cancel works across replicas via Redis pub-sub.
- **Per-end-user bearer auth on HTTP MCP** (deferred from v0.8.15.3). v0.8.15.3 shipped the HTTP MCP transport at `POST /v1/_mcp` with connection-level auth via the operator-static `LOOMCYCLE_AUTH_TOKEN` bearer — same posture as the rest of `/v1/*`. The v0.8.14 `${run.user_bearer}` substitution still flows through the `spawn_run.user_bearer` parameter (so downstream MCP servers see per-end-user identity exactly as for stdio MCP), but the CONNECTION-level auth between the HTTP MCP client and loomcycle is operator-only. Per-end-user HTTP MCP auth — where loomcycle's bearer middleware accepts per-user tokens minted by an upstream identity broker rather than the single operator token — needs design work on token rotation, claims-bearing JWT vs opaque tokens, and the relationship to user_tier resolution. Not a v0.8.x sharp edge; ships when v0.9.x multi-tenant fairness work touches the auth layer.

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

- **Claude Desktop / Claude Code MCP integration** — pre-built `.mcp.json` recipe + a one-page operator guide for adding loomcycle to Claude Code's MCP server list (uses the v0.8.15 LoomCycle MCP surface).
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
