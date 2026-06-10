# CLAUDE.md — loomcycle project guide

This file is loaded by Claude Code on every session in this repo. Read it cold; act from it without re-discovery.

## Project context

**loomcycle** is a high-load agentic runtime: one Go binary that owns the LLM tool-use loop end-to-end (model → tool_use → tool_result → model), runs as a sidecar, and is consumed by application servers over HTTP+SSE / gRPC / MCP / a TS adapter. Multi-provider (Anthropic, OpenAI, DeepSeek, Gemini, Ollama; + a synthetic `code-js` provider). Multi-tenant (per-principal tokens, tenant-isolated state + definition planes). Multi-agent (parent agents spawn sub-agents via the built-in `Agent` tool). Apache-2.0.

It exists because vendor SDKs (`@anthropic-ai/claude-agent-sdk`, OpenAI Agents SDK) bundle a binary, hide the loop, and lock you into one provider. Spawning that binary per call is 20–30 s cold-start, leaks memory under load, and pins a fleet to one model. loomcycle replaces the SDK with a pure HTTP-only loop, exposes native cache-control where the provider supports it, and stays single-tenant-friendly while being shaped to grow into multi-tenant production.

**Where it sits in the stack:**
```
   App / n8n / LangChain  ──HTTP+SSE · gRPC · MCP · TS──▶  loomcycle (this repo)  ──HTTP──▶  Anthropic / OpenAI / DeepSeek / Gemini / Ollama  (+ code-js, mock)
                                  │
                                  ├─ built-in tools (Read/Write/Edit/Grep/Glob/NotebookEdit/HTTP/WebFetch/WebSearch/Bash/Agent/Skill/Memory/Channel/AgentDef/SkillDef/Evaluation/Interruption/Context)
                                  ├─ substrate Defs (Agent/Skill/MCPServer/Schedule/Webhook/MemoryBackend/A2A — runtime-mutable, tenant-scoped)
                                  ├─ MCP servers (stdio pool + HTTP)   ·   MCP server surface (`loomcycle mcp [--upstream]`)
                                  ├─ triggers: Scheduler · inbound Webhooks · A2A peers
                                  └─ LLM Gateway (OpenAI-compatible) · LocalAPI gateway (OpenAPI → tools)
```

**Currently shipped (v0.25.0):** The core primitives stabilised across the v0.8 → v0.23 line; v0.24.0 was an architecture-review hardening pass; v0.25.0 added the manual-management Web UI console + the RFC S ensemble-synchronization primitives (`Context op=time` agent clock, `Channel.await` / `Channel.broadcast` fan-in/out — in-band + MCP + REST/gRPC/TS client twins, `max_fires` self-retiring schedules). This enumeration drifts — treat **README.md "What's shipped"** + **`REVISIONS.md`** as authoritative for per-feature detail, and **`docs/PLAN.md`** for the path forward. The shape today:

- **Six providers, native HTTP, no vendor SDK** — Anthropic, OpenAI, DeepSeek, Gemini, `ollama` (cloud/Bearer), `ollama-local` — behind one `Provider` interface with resolver-based per-tier/effort routing, user-tier fallback cascade, and `PinAfterSuccess`. Plus a synthetic **`code-js`** provider (goja, stateless replay, zero token cost) and a **mock** provider for cost-free load tests.
- **Built-in tools** — Read/Write/Edit/Grep/Glob/NotebookEdit/HTTP/WebFetch/WebSearch/Bash/Agent/Skill/Memory/Channel/AgentDef/SkillDef/Evaluation/Interruption/Context. **Vector Memory** (semantic search; sqlite-vec / pgvector; embedder substrate) and a pluggable **MemoryBackend** (Mem9 add/recall layer).
- **Substrate Defs** — content-addressed (SHA-256), runtime-mutable, tenant-scoped, push-at-boot: Agent / Skill / MCPServer / Schedule / Webhook / MemoryBackend / A2A. Authored over HTTP / gRPC / MCP / the TS adapter; verify-or-fork across deployments.
- **MCP on both sides** — client (mount external servers as tools) and server (`loomcycle mcp [--upstream]` thin client; concurrent stdio dispatch; bounded `spawn_run`; meta-tools).
- **Triggers** — Scheduler (`scheduled_runs`), inbound **Webhooks** (HMAC verify-before-parse), and **A2A** server/client interop. Non-secret run-metadata channel + per-run named credentials across all three trigger surfaces.
- **Multi-tenant authz (RFC L/N)** — `OperatorTokenDef` mints per-principal bearers bound to an authoritative `(tenant, subject, scopes)` resolved from the token; per-route HTTP + per-RPC gRPC scope gates; tenant isolation across BOTH the state plane and the definition plane; role-aware Web UI. `LOOMCYCLE_AUTH_TOKEN` still works — multi-tenancy is available, never required.
- **HA + ops** — Pause/Resume/Snapshot (cross-version-portable JSON), multi-replica (Redis cancel pubsub, cluster pause/resume + bus fanout, DB-backed session locks, singleton sweepers), OTEL + Prometheus + bundled observability profiles, per-tenant fairness, tool-use hooks.
- **Wire surfaces** — HTTP+SSE, gRPC, the `loomcycle mcp` server, the TS adapter (`@loomcycle/client`), an n8n community package, and an LLM Gateway + OpenAI-compatible shims (`POST /v1/_llm/chat`, `/v1/chat/completions`, `/v1/embeddings`).
- **Distribution** — Homebrew + multi-arch Docker; `init` / `doctor` first-run; `loomcycle import claude-code` + the Claude Code plugin; embedded React Web UI at `/ui` (Library, Activity, Channels, Audit, Schedules).

Internal packages added since v0.9.0: `a2a`, `audit`, `auth`, `claudeimport`, `coord`, `hooks`, `recipes`, `resolve`, `runner`, `runstate`, `scheduler`, `snapshot`, `otel`, `pause`.

**LocalAPI gateway** stays scaffolded as a future-consumer convenience for "OpenAPI without an MCP server." The integration vehicle for jobs-search-agent has been the MCP-server pattern since v0.4.0 — jobs-search-agent runs its own `/api/mcp` route exposing typed tools (e.g., `mcp__jobs__getAgentContext`, `mcp__jobs__patchApplication`), and loomcycle consumes it via the existing HTTP MCP transport. Loomcycle stays domain-agnostic; the consumer owns its own surface.

**MCP HTTP-transport hardening (cumulative through v0.8.1)** — surfaced by the jobs-search-agent integration and fixed with regression tests:
1. Operator yaml env-var allowlist must be `LOOMCYCLE_*`-prefixed (or one of the documented third-party names) for `${...}` expansion.
2. `Accept: application/json, text/event-stream` per the Streamable HTTP spec — strict servers return 406 otherwise.
3. SSE-framed responses parsed correctly (the SDK's transport defaults to SSE when both media types are advertised).
4. Pool-startup retry with exponential backoff so a peer compiling its `/api/mcp` route on first request doesn't get marked "skipped" indefinitely.
5. **(v0.8.1)** Lazy retry of "skipped" servers on the first agent call that needs a tool from them — peer restarts no longer require a loomcycle restart.

**Currently planned (→ v1.0):** With the multi-tenant-auth capstone (v0.17.0, RFC L) and the substrate-completeness line (v0.18–v0.23) shipped, **v1.0 is a pure hardening + distribution milestone — no new primitives**: a hardened first-run install story (Homebrew / Docker wired to `init` / `doctor`), a Claude Code plugin robustness pass, and a security + runtime-QA sweep across the v0.13–v0.17 surfaces (A2A, inbound webhooks, pluggable memory, the `code-js` provider, the auth substrate), then the v1.0 tag. Beyond v1.0 (unscheduled): a settings UI, an operator cookbook, Helm. See `docs/PLAN.md` for the public roadmap; decision history lives in the separate operator-side `loomcycle-internal/doc-internal/PLAN.md` (not in this tree).

**Key consumers of loomcycle:**
- `jobs-search-agent` (separate repo; this machine: `/home/denn/workspace/jobs_search_agent`) — first production user. Adopts loomcycle as the only `Runner` backend; CLI / SDK backends were retired in May 2026 after a $80 cost incident traced to subprocess auth inheritance.
- TypeScript client at `adapters/ts/` → `npm: @loomcycle/client`. Python client at `adapters/python/` → `loomcycle` (installable from source; PyPI Trusted Publisher pending).

## Development workflow

This is the chain you follow for every non-trivial change. Don't skip stages; the discipline is what keeps the codebase small and reviewable.

1. **Architect** — read code first. Understand the package layout, the existing patterns, the call graph for the area you're touching. If you can't articulate the seam where your change goes, you're not ready to plan.
2. **Plan** — for anything beyond a one-line fix, write a plan. Include critical files, the change, and a verification step. Validate scope with the user when the change touches the wire protocol, security posture, or storage schema.
3. **Feature branch** — `feature-<short-description>` off `main`. Never commit to `main` directly.
4. **Code** — small focused commits. Each commit should be reviewable in isolation. No "and also fixed this" omnibus commits.
5. **Tests** — unit tests for new code, regression tests for fixes. A regression test is one that *fails on the unfixed code* — verify this by reverting your fix and seeing the test fail before you commit it.
6. **Code review** — self-review pass: read your own diff cold. Look for accidentally-committed secrets, dead code, debug logs, comments that say "TODO" without a follow-up issue.
7. **Regression test** — `go test ./...` from the repo root. Must pass clean. Don't commit "with one known failing test."
8. **Manual tests** — `go build -o bin/loomcycle ./cmd/loomcycle && ./bin/loomcycle --config <yaml>`, then `curl` the affected endpoint. For loop / provider / tool changes, run an actual end-to-end agent run against a real provider key. **For a deployable build (or any Web-UI change) use `make build-all`** — it runs `make build-ui` (embeds the latest SPA into `internal/webui/dist/`) then `make build` (`go build -o bin/loomcycle ./cmd/loomcycle`). Note: plain `go build ./...` is only a compile check — it writes **no** binary, so never use it to produce a deployable `./bin/loomcycle`.
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

2. **Never open or edit `.env.local`.** This file holds API keys and is git-ignored. Reading it surfaces the secrets into your context where they could be accidentally echoed. Editing it could clobber the user's working config. **Exception:** the user explicitly asks you to edit a specific line. In all other cases, when the user asks "set X env var", suggest the line they should add manually (e.g. "Add `LOOMCYCLE_FOO=bar` to `.env.local`") rather than reading or writing the file yourself. The non-secret companion **`.env.insecure`** (the two-file split — secrets vs. operational config, documented in `docs/CONFIGURATION.md` §9c) carries no credentials and *is* safe to read and edit; only `.env.local` is off-limits.

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
