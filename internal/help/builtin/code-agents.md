---
name: code-agents
description: The synthetic code-js provider (RFC J) — running deterministic operator JavaScript as a first-class agent, its JS-side tool API, the default-deny sandbox, and the honest-determinism scope.
---
Some pipeline steps don't need an LLM. ATS scraping, known-shape SQL,
format conversion, payload reshaping, routing — a few lines of code do
the job, deterministically and at zero token cost. The **code-js**
provider makes "code is an agent" first-class: an AgentDef with
`provider: code-js` runs operator-authored JavaScript (via the goja
interpreter) instead of calling a model. From everywhere else in
loomcycle it **is an agent** — same loop, same OTEL spans, same
scheduler / webhook / A2A reachability, same sub-agent composition, same
evaluation surface. Only the cost / determinism profile differs.

## Why a provider, not "just an MCP server"

You can already wrap deterministic work as an MCP server and have an LLM
agent call it. That works — but it makes code a *tool*, not an *agent*,
and breaks substrate symmetry: a ScheduleDef firing a deterministic
pipeline still needs an LLM agent in the middle, paying tokens to
coordinate steps that don't need coordinating. A code-agent IS the unit
the scheduler / webhook / A2A primitives already understand. No LLM in
the loop, no tokens, no hallucination class of bug.

## Enabling

Off by default — operator-provided code runs in the operator's own trust
posture (same as the Bash tool), so you opt in:

```
LOOMCYCLE_CODE_AGENTS_ENABLED=1
LOOMCYCLE_CODE_AGENTS_ROOT=./agent_code              # default; where index.js lives
LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS=120        # wall-clock ctx deadline
LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=0                # seed Date.now/Math.random
```

The JS lives next to your yaml at `agent_code/<agent-name>/index.js`
(mirroring the skills bundling convention). A broken or missing file
fails **at startup**, not at the first scheduled fire.

```yaml
agents:
  nightly-scrape:
    provider: code-js
    allowed_tools: [WebFetch, Memory, mcp__jobs__ingestJobs]  # canonical tool names
    memory_scopes: [user]                                     # required to use Memory.*
    allowed_hosts: ["*.example"]                              # WebFetch host policy
    description: "Deterministic ATS scrape — no LLM."
```

`allowed_tools` uses **canonical** tool names (capitalized, same as LLM
agents); the JS surface naming is in **The JS-side tool API** below.

## Writing a code-agent

Define a top-level `run(input)`. It receives `{prompt, metadata}` and
returns `{final_text, ...}` (or throws). **Tool calls are synchronous** —
no `await`, no callbacks:

```javascript
function run(input) {
  var seen = Memory.get({ scope: "user", key: "seen_ids" }) || {};
  var body = WebFetch({ url: "https://example/api/jobs" });   // built-in tool
  var jobs = parse(body).filter(function (j) { return !seen[j.id]; });
  jobs.forEach(function (j) { seen[j.id] = Date.now(); });
  Memory.set({ scope: "user", key: "seen_ids", value: seen });
  mcp__jobs__ingestJobs({ user_id: input.metadata.user_id, jobs: jobs });  // MCP tool
  return { final_text: "found " + jobs.length + " fresh jobs" };
}
```

You write straight-line code; each tool call returns its result inline. Under
the hood a tool call is one turn of the agent **loop**: the loop dispatches it
(with the loop's hooks, OTEL spans, and `${run.credentials.<name>}`
substitution) exactly as it does an LLM agent's tool_use, and on the next turn
`run()` re-executes, replaying the results already gathered and stopping at the
next call. You never see the replay — calls just look synchronous.

## The JS-side tool API

Only the tools in the agent's `allowed_tools` are bound — **any** allowed
tool, built-in or MCP, is callable. A tool you didn't allow is not a
"permission denied"; it simply **does not exist** in scope
(`ReferenceError`). Default-deny by construction.

| JS | Tool | Notes |
|---|---|---|
| `Memory.get/set/delete/search(obj)` | Memory | multi-op meta-tool; obj is the input minus `op` |
| `Channel.publish/subscribe(obj)` | Channel | multi-op meta-tool; subscribe is a non-blocking peek |
| `Agent.spawn(obj)` | Agent | spawn an LLM (or code) sub-agent; returns its result |
| `WebFetch(obj)` / `Read(obj)` / `HTTP(obj)` / `WebSearch(obj)` / … | the built-in of that name | every other allowed **built-in**, flat by canonical name |
| `mcp__<server>__<tool>(obj)` | that MCP tool | every allowed MCP tool, flat by name |

> **Naming.** You reference a tool in JS by its **exact canonical name** —
> the same string you put in `allowed_tools` and that every other agent uses
> (CamelCase: `Memory`, `WebFetch`, `Read`, …). There is no casing
> translation. The only distinction is shape: the three multi-op meta-tools
> (`Memory`, `Channel`, `Agent`) are **objects** with a method per op
> (`Memory.get(...)`); every other tool is a **flat function**
> (`WebFetch({url})`, `mcp__jobs__ingestJobs({…})`). A name not in
> `allowed_tools` simply isn't defined → `ReferenceError`. `Memory.*`
> additionally needs `memory_scopes` declared (`[agent]` / `[user]`), and
> `WebFetch`/`HTTP` obey the agent's `allowed_hosts` — exactly as for LLM
> agents.

**Return types.** `Memory` / `Channel` / `Agent` and `mcp__*` tools return
**parsed values** — their results are structured JSON, so
`Memory.get(...).value` and `mcp__jobs__getContext(...).foo` just work. The
plain built-ins (`WebFetch`, `Read`, `HTTP`, `Grep`, …) return their **raw
string** result; `JSON.parse(WebFetch(...))` yourself if it's a JSON API.
(Return type follows the tool, never the content — so the same code works
whether a fetched page is JSON or HTML.)

A tool the loop returns as an error surfaces as a **catchable** JS
`throw` (`try { … } catch (e) { … }`); an uncaught throw fails the run
(`code_agent_threw`).

## The sandbox boundary

goja's capability surface IS the boundary. There is **no** ambient `fetch`
/ XHR, no direct filesystem, no `require`, no `setTimeout` / `setInterval`.
`eval` and the `Function` constructor are deleted from the runtime before
your code runs. Capabilities reach the JS **only** as `allowed_tools`
bindings dispatched by the loop: outbound HTTP via the `WebFetch` / `HTTP`
built-ins (or an MCP server) under the agent's `allowed_hosts`; filesystem
via the `Read` tool with operator-configured roots; time-based scheduling
via ScheduleDef. A capability not in `allowed_tools` is simply absent.

> The sandbox protects loomcycle from the *runtime* handing the JS more
> capability than `allowed_tools` granted. It does **not** protect you
> from your own code's logic — that is the operator's trust posture, the
> same as the Bash tool.

## Determinism

The promise is **"no LLM-induced non-determinism"**: zero tokens, no model
latency, no hallucination. Within a run, code-js is deterministic **by
construction** — a code-agent runs as a *replay* over its recorded tool
results, so the ambient clock and RNG are hooked: `Math.random()` is seeded
per run, and `Date.now()` / `new Date()` are anchored to the run's start plus
a per-call offset. Re-executing a run **in the same process** therefore
reproduces the same values — which is what makes a run **resumable** (below).
Different runs still see real-anchored time and fresh entropy, so production
behaviour is unsurprising.

**Cross-process resume caveat.** The default per-run seed is derived from the
run's identity and the anchor from its start time. A resume in a *different*
process (restart, replica handoff) re-derives both from the continuation's own
identity/start, so a code-agent that feeds `Math.random()` / `Date.now()` into
a **tool input** (e.g. a key or idempotency token) would compute a different
input on resume. That no longer corrupts silently: the replay divergence guard
compares each replayed call's input against the recorded one and fails loud
with `code_agent_replay_divergence` rather than feeding a stale result into the
JS. For code-agents that must resume deterministically across processes, set
`LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1` (it pins the seed + anchor to fixed
constants for **all** runs, so the continuation re-derives identical values),
or keep clock/RNG values out of tool inputs (read a real per-call value from a
tool — it is recorded — rather than from `Date.now()`).

This is reproducible *replay*, not "the world stopped": tool / MCP results are
recorded and replayed identically, but the upstream service that produced them
is still whatever it is. `LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1` additionally
freezes the clock + seed across **all** runs — cross-run reproducibility for
tests and snapshot equality.

## Sharp edges

- **Glue-logic fast, not data-processing fast.** goja is interpreted (no
  JIT). CPU-bound work over megabytes belongs in an MCP server. The
  design center is the ~100ms-budget glue step.
- **One tool call at a time.** Parallel tool calls within one code-agent are
  out of v1 (each loop turn advances the replay by one call). Fan out with
  `Agent.spawn(...)`, which is already concurrent at the loomcycle layer.
- **Resumable across restart / replica.** A code-agent holds no in-process
  state between tool calls: each turn re-runs `run()` from the top and
  *replays* the tool results recorded in the run transcript, dispatching only
  the next, not-yet-recorded call. So a run interrupted mid-flight (process
  restart, replica handoff) resumes correctly from the transcript — there is
  no parked continuation to lose. (One caveat for cross-*process* resume: a
  code-agent that derives a tool input from `Math.random()`/`Date.now()` needs
  `LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1` to re-derive identical inputs — see
  **Determinism** above.) The cost is that the pure-JS portion
  re-executes each turn (≈O(N²) for N sequential tool calls), which is fine
  for the glue-logic design center; heavy compute belongs in an MCP server.
  (This is why determinism is always-on above — replay must reproduce the
  same call sequence; a non-deterministic divergence fails loud as
  `code_agent_replay_divergence`.)
- **MaxIterations is a hard sequential-tool-call ceiling.** Each loop turn
  advances a code-agent's replay by exactly one tool call, so the run's
  `MaxIterations` caps how many sequential tool calls `run()` may make — it is
  not a soft "model chatter" cap as it is for an LLM agent. A code-agent that
  needs more sequential calls than the cap ends with `stop_reason:
  max_iterations` (and an operator log line naming the agent + cap). Raise
  `MaxIterations` for that run, or fan out concurrent work via `Agent.spawn`.
- **Run timeout bounds wall time.** A CPU-bound JS loop is cut by goja
  `Interrupt` at the per-turn timeout; the overall run deadline rides the
  loop's ctx. Set `LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS`. The heap limit
  is best-effort (goja exposes no hard cap).
- **ABI versioning.** The JS-side API is versioned on its own semver
  (currently 1.0.0), separate from loomcycle's release vector. Breaking a
  signature is a major bump with a deprecation window.
