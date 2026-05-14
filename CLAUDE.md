# CLAUDE.md — loomcycle project guide

This file is loaded by Claude Code on every session in this repo. Read it cold; act from it without re-discovery.

## Project context

**loomcycle** is a high-load agentic runtime: one Go binary that owns the LLM tool-use loop end-to-end (model → tool_use → tool_result → model), runs as a sidecar, and is consumed by application servers over a small HTTP+SSE API. Multi-provider (Anthropic, OpenAI, Ollama). Multi-tenant. Multi-agent (parent agents spawn sub-agents via the built-in `Agent` tool). Apache-2.0.

It exists because vendor SDKs (`@anthropic-ai/claude-agent-sdk`, OpenAI Agents SDK) bundle a binary, hide the loop, and lock you into one provider. Spawning that binary per call is 20–30 s cold-start, leaks memory under load, and pins a fleet to one model. loomcycle replaces the SDK with a pure HTTP-only loop, exposes native cache-control where the provider supports it, and stays single-tenant-friendly while being shaped to grow into multi-tenant production.

**Where it sits in the stack:**
```
   App server  ──HTTP/SSE──▶  loomcycle (this repo)  ──HTTP──▶  Anthropic / OpenAI / Ollama
                                  │
                                  ├─ built-in tools (Read/Write/Edit/HTTP/WebFetch/WebSearch/Bash/Agent/Skill/Memory/Channel/AgentDef/Evaluation/Context)
                                  ├─ MCP servers (stdio pool + HTTP)
                                  └─ LocalAPI gateway (OpenAPI → tools)
```

**Currently shipped (v0.8.15):** six provider registrations (Anthropic, OpenAI, DeepSeek, Gemini, `ollama` cloud / Bearer auth, `ollama-local`), **fourteen built-in tools** (Read, Write, Edit, HTTP, WebFetch, WebSearch, Bash, Agent, Skill, Memory, Channel, **AgentDef**, **Evaluation**, **Context**), MCP integration (stdio + Streamable HTTP, with lazy-retry on first agent call so a peer that was unreachable at boot self-heals on demand), static skill bundling (Approach A), agent directory discovery (yaml `agents:` map merges with `<name>.md` files under `LOOMCYCLE_AGENTS_ROOT` — yaml acts as override layer), named `user_tiers` with resolver overlay + runtime fallback (per-user-tier provider/model policy; runs carry `user_tier` per request; retryable errors (429/5xx/network/stream-idle) trigger provider switch within the tier's candidate list, capped at 3 cumulative attempts; per-tier `fallback_on_error` opts free tiers out of the cascade), **persistent `Channel` tool** (inter-agent message bus; publish/subscribe/ack/peek/list_channels; cursor-based at-least-once delivery; in-process notification bus for long-poll; operator-yaml ACL with prefix wildcards; bounded storage with lossy-on-overflow), **Self-Evolution Substrate** (v0.8.5: `AgentDef` tool with 6 ops + versioned `agent_defs` storage with lineage + `Evaluation` tool with 5 ops + sub-agent `def_id` pinning; selection stays policy, loomcycle does NOT auto-promote), **system channels + deferred publish** (v0.8.6: operator-declared `_system/*` namespace with heartbeats/alarms/runtime-state/provider-events; `SystemPublisher` interface; bearer-authed admin endpoint `POST /v1/_channels/_system/{name}`; general `deliver_at` on any channel's publish with `(visible_at, msg_id)` tuple cursor), **`Context` tool** (v0.8.7: 10 read-only ops covering self/tools/doc/permissions/agents/lineage/evaluations/channels/history + v0.8.8 `help` for narrative cross-cutting topics with bundled FS embed + `LOOMCYCLE_HELP_ROOT` filesystem overlay; auto-attached to every agent's `allowed_tools` at config-load, opt out via `disable_context: true`), **Gemini schema sanitizer** (v0.8.10: `$ref` inlining + `oneOf`/`anyOf`/`allOf` merge — fixes 400 INVALID_ARGUMENT on Zod-discriminatedUnion MCP tool schemas), **process-resource metrics sampler** (v0.8.11: built-in periodic CPU + memory sampler with idle-gating on `Semaphore.Stats()`; persists to `process_samples` table; 3 bearer-authed `/v1/_metrics/*` endpoints for windowed sample lists, per-run rollups via SQL JOIN, and aggregated 1h/24h/7d buckets; default OFF, opt-in via `LOOMCYCLE_METRICS_ENABLED=1`), **cross-provider `reasoning_content` strip on fallback** (v0.8.12: when `tryProviderFallback` switches providers mid-conversation, walks the in-flight messages slice and zeroes `Message.Reasoning` on every assistant turn so the new provider gets a clean history; fixes the DeepSeek 400 `"reasoning_content must be passed back"` when falling back from a thinking-capable provider; emits new `EventReasoningInvalidated` typed event mirroring the v0.8.2 `EventCacheInvalidated` pattern), **pin provider after first successful turn** (v0.8.13: `FallbackPolicy.PinAfterSuccess bool` + `LOOMCYCLE_FALLBACK_PIN_AFTER_SUCCESS` env var + new `EventFallbackSuppressed` typed event; suppresses cross-provider mid-conversation fallback after turn 1 succeeds, closing the entire class of transcript-translation bugs that v0.8.12 was patching one-by-one; default OFF in v0.8.x, default-on planned for v0.9.x), **per-run MCP bearer tokens** (v0.8.14: `${run.user_bearer}` and `${run.user_bearer:-FALLBACK}` substitution in operator yaml `mcp_servers.*.headers`; new `user_bearer` wire field on `runRequest` + `messagesRequest`; ctx-carried via `tools.RunIdentityValue.UserBearer` and inherited identically by sub-agents; substitution happens per-request in `Client.do()` against a local map copy — `c.headers` is never mutated, so concurrent runs send distinct bearers without coordination), **auto-version from runtime/debug** (v0.8.14: `--version` reports `version=<v> commit=<c> built=<t> go=<g>` derived from Go's embedded VCS stamp; no ldflags required; release scripts can still override via `-X main.buildVersion=...`), **LoomCycle MCP server** (v0.8.15: `loomcycle mcp --config Y` subcommand exposes loomcycle itself as a stdio MCP server with 20 meta-tools — spawn_run / cancel_run / get_run / list_runs / register_agent / unregister_agent / list_agents / memory / channel / agentdef / evaluation / context plus PREVIEW-mocked pause_runtime / resume_runtime / get_runtime_state / create_snapshot / list_snapshots / export_snapshot / restore_snapshot / delete_snapshot; new `connector.Connector` Go interface is the architectural anchor — HTTP server is the canonical implementation, MCP + gRPC + future CLI all CONSUME it via direct method dispatch; streaming via `notifications/loomcycle/run_event` when client opts in via `initialize.capabilities.loomcycle.runEvents=true`; dynamic agents persist to new `dynamic_agents` table with TTL sweeper; Bash/Write/Edit stripped from dynamic agent `allowed_tools` unless `LOOMCYCLE_MCP_ALLOW_PRIVILEGED_TOOLS=1`; companion `loomcycle-mcp.sh` wrapper sources `.env.local` for Claude Code integration), agent tracking + cancel API with parent-child cascade, sub-agent caller-host policy inheritance, per-agent `max_tokens`, per-stream provider timeouts (`ResponseHeaderTimeout` + per-byte idle), SQLite + Postgres stores, semaphore-based concurrency, embedded React Web UI at `/ui`, persistent `Memory` tool (agent + user scopes, atomic `incr`, TTL).

**LocalAPI gateway** stays scaffolded as a future-consumer convenience for "OpenAPI without an MCP server." The integration vehicle for jobs-search-agent has been the MCP-server pattern since v0.4.0 — jobs-search-agent runs its own `/api/mcp` route exposing typed tools (e.g., `mcp__jobs__getAgentContext`, `mcp__jobs__patchApplication`), and loomcycle consumes it via the existing HTTP MCP transport. Loomcycle stays domain-agnostic; the consumer owns its own surface.

**MCP HTTP-transport hardening (cumulative through v0.8.1)** — surfaced by the jobs-search-agent integration and fixed with regression tests:
1. Operator yaml env-var allowlist must be `LOOMCYCLE_*`-prefixed (or one of the documented third-party names) for `${...}` expansion.
2. `Accept: application/json, text/event-stream` per the Streamable HTTP spec — strict servers return 406 otherwise.
3. SSE-framed responses parsed correctly (the SDK's transport defaults to SSE when both media types are advertised).
4. Pool-startup retry with exponential backoff so a peer compiling its `/api/mcp` route on first request doesn't get marked "skipped" indefinitely.
5. **(v0.8.1)** Lazy retry of "skipped" servers on the first agent call that needs a tool from them — peer restarts no longer require a loomcycle restart.

**Currently planned (v0.8.x → v1.0 outline):** Question tool (human-in-the-loop primitive, v0.8.16), Pause / Resume / Snapshot (runtime-wide quiesce + cross-version-portable JSON snapshot; precondition for v0.9.x multi-replica HA, v0.8.17), high-load capacity work (per-tenant fairness, OTEL, run-status cache, heartbeat sweeper hardening — v0.9.x). See `docs/PLAN.md` for the public roadmap and `~/work/loomcycle-internal/doc-internal/PLAN.md` for decision history (lives in a separate operator-side repo).

**Key consumers of loomcycle:**
- `jobs-search-agent` (separate repo at `/Users/denn/work/jobs-search-agent`) — first production user. Adopts loomcycle as the only `Runner` backend; CLI / SDK backends were retired in May 2026 after a $80 cost incident traced to subprocess auth inheritance.
- TypeScript client at `adapters/ts/` → `npm: @loomcycle/client`. Python client deferred.

## Development workflow

This is the chain you follow for every non-trivial change. Don't skip stages; the discipline is what keeps the codebase small and reviewable.

1. **Architect** — read code first. Understand the package layout, the existing patterns, the call graph for the area you're touching. If you can't articulate the seam where your change goes, you're not ready to plan.
2. **Plan** — for anything beyond a one-line fix, write a plan. Include critical files, the change, and a verification step. Validate scope with the user when the change touches the wire protocol, security posture, or storage schema.
3. **Feature branch** — `feature-<short-description>` off `main`. Never commit to `main` directly.
4. **Code** — small focused commits. Each commit should be reviewable in isolation. No "and also fixed this" omnibus commits.
5. **Tests** — unit tests for new code, regression tests for fixes. A regression test is one that *fails on the unfixed code* — verify this by reverting your fix and seeing the test fail before you commit it.
6. **Code review** — self-review pass: read your own diff cold. Look for accidentally-committed secrets, dead code, debug logs, comments that say "TODO" without a follow-up issue.
7. **Regression test** — `go test ./...` from the repo root. Must pass clean. Don't commit "with one known failing test."
8. **Manual tests** — `go build -o bin/loomcycle ./cmd/loomcycle && ./bin/loomcycle --config <yaml>`, then `curl` the affected endpoint. For loop / provider / tool changes, run an actual end-to-end agent run against a real provider key.
9. **PR** — one branch, one PR. Title says what the change does in one line. Body explains *why* (the user-visible problem), *what* (the technical change), and *what was tested* (the test names + the manual verification steps).
10. **Human review** — wait for the user. Do not self-merge. Address review comments in additional commits on the same branch (don't force-push unless the user asks).
11. **Merge** — squash to `main` after approval, with the PR title as the commit subject and the PR body as the commit body. Tag if it's a release.
12. **CI** - keep CI always actual, update tests and make sure all CI tests are passed on every PR.

Skip the chain only for trivial fixes (typos, stale comment lines, obviously-correct one-liners). When in doubt, follow the chain.

## Karpathy guidelines (coding discipline)

These come from the `andrej-karpathy-skills:karpathy-guidelines` skill. Embed the gist here so they're available without invoking the skill.

- **Surgical changes.** Modify the minimum surface needed to fix or add what's asked. Don't rewrite adjacent code "while you're there." Don't refactor on the same commit as a behavior change. The smaller the diff, the easier the review.
- **Surface assumptions.** When you make a non-obvious choice, write it down in a comment or the PR body. "I assumed N is always small enough that O(N²) is fine" is the kind of statement that prevents future surprises.
- **Verifiable success criteria.** Before writing code, write down how you'll know it works. A regression test, a curl + expected output, a metric — something concrete. "It compiles" is not a success criterion.
- **Avoid overcomplication.** Prefer the boring solution. A 30-line stdlib implementation beats a 3-line dependency. Don't introduce abstractions speculatively.
- **Don't trust your own first draft.** After writing a function, read it as if someone else wrote it. Most bugs are obvious on the second read.

## Security rules — non-negotiable

These rules apply to every interaction with this repo. Violations are bugs even when they don't produce visible failure.

1. **Never share secrets.** API keys, bearer tokens, OAuth tokens, signing keys — never paste them into responses, tool output, log lines, or commit messages. If you need to refer to one, reference the env var name (e.g. `ANTHROPIC_API_KEY`) without the value.

2. **Never open or edit `.env.local`.** This file holds API keys and is git-ignored. Reading it surfaces the secrets into your context where they could be accidentally echoed. Editing it could clobber the user's working config. **Exception:** the user explicitly asks you to edit a specific line. In all other cases, when the user asks "set X env var", suggest the line they should add manually (e.g. "Add `LOOMCYCLE_FOO=bar` to `.env.local`") rather than reading or writing the file yourself.

3. **Ignore API keys you happen to see.** If a key appears in a log line, a tool result, a stack trace, a stash diff — do not echo it back, do not include it in a commit, do not reference it. Pretend you didn't see it. If you must reference it (e.g. "the OPENAI_API_KEY env var is missing"), reference by name only.

4. **Treat these env-var name patterns as secrets:** anything matching `*_KEY`, `*_TOKEN`, `*_SECRET`, `*_AUTH`, `*_PASSWORD`, `*_CREDENTIAL`. The `LOOMCYCLE_AUTH_TOKEN` env var is the bearer used by all `/v1/*` endpoints — never log or echo its value.

5. **Targeted git adds, never `git add .` or `git add -A`** when you have any uncertainty about what's staged. `.env*`, `*.pem`, `*.key` — these can sweep into a commit if you stage with a wildcard. Use `git add <specific paths>` and check `git status --short` before commit. (Internal design docs used to live at `doc-internal/` and were gitignored; v0.8.15 migrated them to `~/work/loomcycle-internal/doc-internal/` outside this repo.)

6. **Bearer-token check is constant-time.** If you touch the auth middleware in `internal/api/http/`, preserve the constant-time `subtle.ConstantTimeCompare` pattern. Variable-time comparison is a side-channel.

7. **The Bash tool is NOT a sandbox.** Even with `LOOMCYCLE_BASH_ENABLED=1`, the tool can reach arbitrary files via absolute paths and make network calls. Operators wanting real isolation must run loomcycle inside a container or VM. If you're modifying Bash, do not weaken its existing protections (cwd-binding, env-scrubbing, output bounds, timeouts) without an explicit user request.

8. **Caller-authoritative `allowed_hosts` is a trust boundary.** It must come from the upstream caller, never from the model. Don't pipe model-generated text into the policy. If you change `internal/tools/builtin/narrowing.go` or `internal/api/http/server.go runRequest`, preserve the "operator's static list is the floor" invariant.

## Code conventions

- **Go style:** `gofmt` clean. `errcheck` clean. No package-level mutable globals (config struct passed in, not read from a singleton). Errors-as-values; no panics outside `main` and intentional `panic("unreachable")` sites. Logging via `log` package (structured logging is a v1.x consideration).
- **Test naming:** `Test<Subject>_<Behaviour>`. e.g. `TestSubAgent_InheritsParentCallerHostAllowlist`. The behaviour describes the postcondition the test asserts, not the input.
- **Commit messages:** subject ≤ 72 chars in imperative mood ("fix(api): propagate host policy to sub-agents"). Body explains *why* before *what*. Reference commits via short SHA when calling out a regression. Always close with the `Co-Authored-By` line if Claude wrote substantial code.
- **Comments explain WHY, not WHAT.** Code says what; comments add the missing context (the constraint, the past incident, the surprising-but-correct invariant). Don't write comments that summarize the next line.
- **No backward-compat shims for unused code.** When deleting a function, delete it; don't keep a stub. When deprecating an env var, delete the read; don't add a "deprecated, ignored" branch.

## Cross-repo notes

loomcycle and `jobs-search-agent` (its primary downstream consumer) are separate repos with linked release cadences:

- **Wire-protocol changes** (HTTP request/response shape, SSE event types, YAML schema) require updating both repos in lockstep. Land in loomcycle first; bump the dependency in jobs-search-agent only after the loomcycle change is on `main`.
- **Runtime-only changes** (provider drivers, internal pkgs, performance fixes that don't change the wire) ship from loomcycle alone; jobs-search-agent picks them up on the next loomcycle bump.
- **Agent prompt changes** (`.claude/agents/*.md` files in jobs-search-agent) are a jobs-search-agent concern; loomcycle is agent-agnostic.
- **The TS adapter** (`adapters/ts/`) is published as `@loomcycle/client`. Changes to it require a version bump on the npm package. jobs-search-agent depends on a pinned version.

## When in doubt

- Look at recent commits on `main` (`git log --oneline -20`) for the style of recent changes.
- Read `~/work/loomcycle-internal/doc-internal/PLAN.md` "Decisions (locked)" section before introducing a new architectural pattern (lives in a separate operator-side repo).
- Ask the user. A 30-second clarification beats an hour of speculative refactor.
