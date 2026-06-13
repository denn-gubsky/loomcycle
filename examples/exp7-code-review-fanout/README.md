# exp7 — delegate a real code review to loomcycle (MCP fan-out)

An external orchestrator (Claude Code, or the `work/exp7_run.sh` driver here) **delegates a whole
code review to loomcycle and fans the reviewers out _through the MCP server_** — one
[`spawn_runs`](#the-primitive-spawn_runs-rfc-y) call spawns **10 read-only `code-reviewer` agents**
concurrently over repo slices; each records its confidence-scored findings to **Memory**; then one
`spawn_run` runs a **consolidator** that merges the ledger and returns the executive report. The
orchestrator's own context never reads the code — loomcycle's agents do.

It exercises loomcycle as a **runtime for someone else's agents**: import a Claude Code agent + skill
(`loomcycle import claude-code`), fan it out natively over MCP (RFC Y `spawn_runs`), collect via
Memory, consolidate, return.

```
 ORCHESTRATOR (Claude Code / ./work/exp7_run.sh)        loomcycle MCP thin client: `loomcycle mcp --upstream`
   step 1.  git clone <a Go repo> → work/loomcycle-src   (read-only review target, inside the jail)
   step 2.  ONE spawn_runs  (MCP, mode=join, N=10)  ─────────────────────────►  loomcycle runtime
              spawns=[ {agent:code-reviewer, prompt: slice=<s> path=<p>} ×10 ]     (server-side concurrent,
              join AND-barrier: blocks until all 10 settle → index-aligned envelope  bounded by admission gate)
                    │
                    ▼
              10 × code-reviewer (one per slice), each:
                Glob/Grep/Read loomcycle-src/<path> (READ-ONLY, relative to read-root)
                → Memory.set review:<slice>:findings   (confidence ≥ 80 only, file:line + fix)
   step 3.  ONE spawn_run  (MCP) → exp7-consolidator
              Memory.list "review:" → merge + dedup + count → Memory.set consolidated:report → RETURN
   step 4.  orchestrator reads the report (consolidator final_text / consolidated:report)
```

The `spawn_runs` join barrier **is** the fan-in — there is no in-loomcycle dispatcher agent and no
fan-in channel. (loomcycle < v0.32.0 had no `spawn_runs`; the original sandbox exp7 used an
in-loomcycle dispatcher + `Agent op=parallel_spawn` as the workaround — that motivated RFC Y, shipped
in v0.32.0. See [the note below](#a-note-on-the-sandbox-origin--rfc-y).)

## What it demonstrates

| Primitive | Role |
|---|---|
| **`loomcycle import claude-code`** | bring a Claude Code `code-reviewer` agent + a `code-review` skill into loomcycle yaml (`tools`→`allowed_tools`, body→`system_prompt`, skill body bundled at load) |
| **`spawn_runs` (RFC Y, MCP/`POST /v1/runs:batch`)** | the **external fan-out** — ONE call spawns N runs server-side-concurrent, `mode:join` blocks until all settle, returns an index-aligned envelope (a per-child failure is captured in-envelope, never fails the batch) |
| **`spawn_run` (MCP)** | the single consolidator run after the fan-out |
| **`Memory` (user scope)** | the shared findings ledger — `review:<slice>:findings` per reviewer → `consolidated:report` |
| **read-only `Read`/`Grep`/`Glob` jail** | reviewers get the file tools sandboxed to `LOOMCYCLE_READ_ROOT` with **no Bash/Write** — they can read the repo but not mutate or execute |
| **`Context op=self`** | each reviewer self-reports its `run_id` into its findings (provenance) |

Routing: **Anthropic OAuth (primary) → deepseek-v4-pro (fallback)** via `tier: middle`.

## Prerequisites
- **loomcycle ≥ v0.32.0** on PATH (or `LOOMCYCLE_BIN`) — `spawn_runs` shipped in v0.32.0. Check:
  `loomcycle --version`.
- **A provider** — Anthropic OAuth (`loomcycle anthropic login`, kept enabled by `run.sh`) or
  `DEEPSEEK_API_KEY` in `.env.local`.
- **`git`** (to clone the review target) and **`python3`** (the driver + MCP client are stdlib-only).

⚠️ Not fully self-contained: you clone a Go repo to review (step 1). Everything else is local — no
external services, no MCP servers to install, no scheduler/webhooks.

## Step 1 — clone the review target into the jail
The reviewers read `work/loomcycle-src/<slice>` (relative to the read-root `./work`). Clone any Go
repo there; the default slices match **loomcycle's own** layout, so reviewing loomcycle is the
turnkey path:
```bash
cd examples/exp7-code-review-fanout
git clone --depth 1 https://github.com/denn-gubsky/loomcycle work/loomcycle-src
# (or point at your own checkout: cp -r /path/to/loomcycle work/loomcycle-src — Go repo, the 10
#  default slices are internal/api/http, internal/tools/builtin, …, cmd/loomcycle; edit them in
#  work/exp7_run.sh's SLICES list if your repo differs.)
```

## Step 2 — run + drive

> **`loomcycle validate` note:** this is tier-based config (for the OAuth→deepseek fallback), so
> `validate` reports `no provider resolved` — it doesn't probe providers. Not a config bug; verify by
> **running** and watching the boot `resolve probe:` lines (and `skills: loaded 1`).

Terminal 1 — start the server:
```bash
cd examples/exp7-code-review-fanout
./run.sh        # first launch copies .env.local.example → .env.local; add a provider, re-run
```
You should see `skills: loaded 1 from …/skills` and `loomcycle listening on 127.0.0.1:8787`.

Terminal 2 — **smoke-test the MCP path** (one reviewer, the small `pause` slice), then run the full
delegation:
```bash
cd examples/exp7-code-review-fanout
./work/exp7_run.sh smoke        # spawn_runs N=1 → validate the MCP fan-out end-to-end (~1-2 min)
./work/exp7_run.sh delegate     # spawn_runs N=10 + the consolidator (~8-15 min; see the throttle note)
```
`delegate` prints the per-child envelope (each reviewer's `run_id` + status + its `REVIEWED` line),
then the consolidator's returned report. Expect something like:
```
[fanout] caller -> MCP spawn_runs: fanning out 10 code-reviewer run(s) (mode=join)...
  envelope: 10 child result(s)
    completed run=r_…  REVIEWED slice=api-http files=15 issues=4
    completed run=r_…  REVIEWED slice=store    files=12 issues=7
    …
[consolidate] caller -> MCP spawn_run: exp7-consolidator …
  --- consolidated:report (from Memory) ---
  { "slices_reviewed":"10/10", "files_reviewed":86, "total_issues":35,
    "by_severity":{"Critical":1,"Important":34}, "top_findings":[ … ], "summary":"…" }
```
(Exact counts vary — the reviewers are LLM agents over a real repo.)

### How the fan-out goes "through MCP"
`work/exp7_mcp.py` launches `loomcycle mcp --upstream http://127.0.0.1:8787` (the same thin client
that backs the Claude Code loomcycle plugin) and speaks JSON-RPC to it over stdio — `initialize` →
`tools/call spawn_runs`. The thin client proxies to the runtime's `/v1/_mcp`. The upstream bearer is
passed via the `LOOMCYCLE_MCP_UPSTREAM_TOKEN` env (never argv/printed); empty = open mode.

**Three equivalent ways to drive the same `spawn_runs` handler:**
1. **This driver** (`work/exp7_run.sh`) — MCP stdio thin client, no Claude Code needed. ← default
2. **Claude Code's loomcycle plugin** — `/loomcycle:connect --base-url=http://127.0.0.1:8787` then
   `/loomcycle:fanout code-reviewer --count=10 <prompt>` (the `mcp__loomcycle__spawn_runs` tool).
3. **REST** — `POST /v1/runs:batch` with `{spawns:[…], mode:"join"}` via `./loomcurl.sh`.

## Verify (independent re-derivation — don't trust the consolidator's narration)
`delegate` ends by leaving the ledger in the store; re-derive it any time:
```bash
./work/exp7_run.sh verify
```
It reads straight from the store and checks:
- **Coverage** — how many of the 10 `review:<slice>:findings` recorded (and per-slice file/issue counts).
- **The report is grounded** — `consolidated:report` only aggregates what the reviewers recorded.
- Each finding cites a real `file:line` you can open in `work/loomcycle-src` and confirm.

By hand:
```bash
BASE=http://127.0.0.1:8787
./loomcurl.sh "$BASE/v1/_memory/scopes/user/exp7/keys?prefix=review:&limit=100"   # the per-slice ledger
./loomcurl.sh "$BASE/v1/_memory/scopes/user/exp7/keys/consolidated:report"        # the returned report
```
**Read-only safety:** reviewers have `[Read, Grep, Glob, Memory, Context]` — no Bash/Write; nothing
under `work/loomcycle-src` is ever modified.

## The throttle (rate limits) — important
10 concurrent sonnet reviewers each reading 10–15 files can **saturate a subscription (OAuth) account
rate limit** (HTTP 429). So `loomcycle.yaml` sets `concurrency.max_concurrent_runs: 4` (+ a long
`queue_timeout_ms`): the `spawn_runs` join still accepts all 10 — the other 6 **queue** and run as
slots free. If you drive on a **paid API key** with more headroom, raise `max_concurrent_runs` for a
faster wall-clock. (`queue_timeout_ms` must exceed a reviewer's runtime, or queued children are
rejected instead of waiting.)

## Provenance — how `code-reviewer` was imported
`./claude-code-src/agents/code-reviewer.md` + `./claude-code-src/skills/code-review/SKILL.md` are the
Claude Code source artifacts (a normal `.claude`-layout dir — `agents/` + `skills/` — renamed only
because the loomcycle repo gitignores `.claude/`; the importer reads the structure, not the name).
They were imported into `loomcycle.yaml` with:
```bash
loomcycle import claude-code --from=./claude-code-src --write --diff=./loomcycle.yaml --skills-dest=./skills
```
The importer maps `tools`→`allowed_tools` and the body→`system_prompt`, and copies the skill to
`./skills/code-review/SKILL.md` (bundled into the reviewer's prompt at load via `LOOMCYCLE_SKILLS_ROOT`).
Two manual touch-ups after import (both noted in `loomcycle.yaml`): the importer **drops the agent's
`skills:` frontmatter**, so `skills:[code-review]` is re-attached; and `model: sonnet` (a Claude-Code
alias) is replaced with `tier: middle`. The shipped `loomcycle.yaml` is the post-import result, ready
to run — you don't need to re-run the import unless you change the source artifacts.

## Tuning
- **Slices.** The 10 `(slice, path)` pairs live in `work/exp7_run.sh`'s `SLICES` list (and the
  consolidator's expected-keys list in `loomcycle.yaml`). Edit them to match your repo, or cut the
  list down for a cheaper run.
- **Reviewer budget.** `code-reviewer.max_iterations` (40) / `max_tokens` (8192) — raise for very
  large slices.
- **Model.** `tier: middle` = sonnet → deepseek. Drop the reviewers to `tier: low` (haiku/flash) for
  a cheaper, faster pass.

## Teardown
Ctrl-C the server; `rm -rf data` for a clean ledger; `rm -rf work/loomcycle-src` to drop the clone.

## Files

| File | Purpose |
|---|---|
| `loomcycle.yaml` | routing + `code-reviewer` (imported, read-only) + `exp7-consolidator`; throttled concurrency |
| `run.sh` | launcher — sets the read-only jail (`LOOMCYCLE_READ_ROOT=./work`) + skills root + MCP upstream token |
| `claude-code-src/{agents/code-reviewer.md,skills/code-review/SKILL.md}` | the Claude Code source artifacts the import was produced from (provenance; `.claude`-layout) |
| `skills/code-review/SKILL.md` | the imported skill body, bundled into the reviewer's prompt at load |
| `work/exp7_mcp.py` | token-safe MCP stdio JSON-RPC client driving `loomcycle mcp --upstream` |
| `work/exp7_run.sh` | the driver — `smoke` / `fanout` / `consolidate` / `delegate` / `verify` |
| `loomcurl.sh` | token-safe loomcycle REST helper (omits the bearer in dev open mode) |
| `.env.local.example` | secret template (empty) — bearer + provider only |

## A note on the sandbox origin + RFC Y
This is the **static, self-contained** form of loomcycle sandbox **Experiment #7**. The original
exp7 ran before `spawn_runs` existed and had to fan out with an **in-loomcycle dispatcher agent**
(`Agent op=parallel_spawn`), because an external caller couldn't fan out N runs over MCP in one call
— N concurrent blocking calls serialise over a single MCP connection. That gap was filed as **RFC Y
(external fan-out run)** and **shipped in v0.32.0** as `spawn_runs` (MCP) + `POST /v1/runs:batch`
(REST), `mode:join`, cap 32, reusing the in-loop `parallel_spawn` core at the external boundary. This
example is the post-RFC-Y topology: the dispatcher is gone; the fan-out happens at the MCP boundary.
