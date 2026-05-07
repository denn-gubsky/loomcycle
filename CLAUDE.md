# CLAUDE.md — loomcycle project guide

This file is loaded by Claude Code on every session in this repo. Read it cold; act from it without re-discovery.

## Project context

**loomcycle** is a high-load agentic runtime: one Go binary that owns the LLM tool-use loop end-to-end (model → tool_use → tool_result → model), runs as a sidecar, and is consumed by application servers over a small HTTP+SSE API. Multi-provider (Anthropic, OpenAI, Ollama). Multi-tenant. Multi-agent (parent agents spawn sub-agents via the built-in `Agent` tool). Apache-2.0.

It exists because vendor SDKs (`@anthropic-ai/claude-agent-sdk`, OpenAI Agents SDK) bundle a binary, hide the loop, and lock you into one provider. Spawning that binary per call is 20–30 s cold-start, leaks memory under load, and pins a fleet to one model. loomcycle replaces the SDK with a pure HTTP-only loop, exposes native cache-control where the provider supports it, and stays single-tenant-friendly while being shaped to grow into multi-tenant production.

**Where it sits in the stack:**
```
   App server  ──HTTP/SSE──▶  loomcycle (this repo)  ──HTTP──▶  Anthropic / OpenAI / Ollama
                                  │
                                  ├─ built-in tools (Read/Write/Edit/HTTP/WebFetch/WebSearch/Bash/Agent/Skill)
                                  ├─ MCP servers (stdio pool + HTTP)
                                  └─ LocalAPI gateway (OpenAPI → tools)
```

**Currently shipped (v0.3.9, working toward v0.4.0):** all three providers, eight built-in tools (Read, Write, Edit, HTTP, WebFetch, WebSearch, Bash, Agent, Skill), MCP integration (stdio + HTTP), static skill bundling (Approach A), agent tracking + cancel API with parent-child cascade, sub-agent caller-host policy inheritance, per-agent `max_tokens`, SQLite store, semaphore-based concurrency. Wire shape is HTTP+SSE; gRPC is deferred.

**LocalAPI MCP gateway** is the v0.4.0 blocking item: code + tests landed in `internal/tools/localapi/`, wired into `cmd/loomcycle/main.go` via `cfg.LocalAPI.SpecPath`, but no production spec exists yet. The end-to-end migration (jobs-search-agent's nine HTTP-using agents from raw `HTTP` tool with hand-written URLs to typed `localapi__jobs__<op>` tools) defines v0.4.0's release gate.

**Currently planned (v1.0 outline):** Memory tool (agent-scoped persistent storage), Channel tool (inter-agent message bus), LoomHelp tool (runtime introspection), LoomCycle MCP (loomcycle exposes itself as an MCP server so external orchestrators can spawn/configure agents), high-load capacity work (per-tenant fairness, Postgres `Store`, OTEL, run-status cache), web monitoring frontend. See `docs/PLAN.md` for the public roadmap and `doc-internal/PLAN.md` for decision history.

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

5. **Targeted git adds, never `git add .` or `git add -A`** when you have any uncertainty about what's staged. `.env*`, `*.pem`, `*.key`, anything under `doc-internal/` (gitignored intentionally) — these can sweep into a commit if you stage with a wildcard. Use `git add <specific paths>` and check `git status --short` before commit.

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
- Read the doc-internal/PLAN.md "Decisions (locked)" section before introducing a new architectural pattern.
- Ask the user. A 30-second clarification beats an hour of speculative refactor.
