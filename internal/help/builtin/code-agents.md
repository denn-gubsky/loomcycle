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
    memory_scopes: [user]                                     # required to use memory.*
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
  var seen = memory.get({ scope: "user", key: "seen_ids" }) || {};
  var body = WebFetch({ url: "https://example/api/jobs" });   // built-in tool
  var jobs = parse(body).filter(function (j) { return !seen[j.id]; });
  jobs.forEach(function (j) { seen[j.id] = Date.now(); });
  memory.set({ scope: "user", key: "seen_ids", value: seen });
  mcp__jobs__ingestJobs({ user_id: input.metadata.user_id, jobs: jobs });  // MCP tool
  return { final_text: "found " + jobs.length + " fresh jobs" };
}
```

Under the hood each tool call transparently suspends the JS while the
agent **loop** dispatches it — with the loop's hooks, OTEL spans, and
`${run.credentials.<name>}` substitution — then resumes with the result.
The mechanics are identical to an LLM agent's tool-use turns; you just
write straight-line code.

## The JS-side tool API

Only the tools in the agent's `allowed_tools` are bound — **any** allowed
tool, built-in or MCP, is callable. A tool you didn't allow is not a
"permission denied"; it simply **does not exist** in scope
(`ReferenceError`). Default-deny by construction.

| JS | Tool | Notes |
|---|---|---|
| `memory.get/set/delete/search(obj)` | Memory | multi-op meta-tool; obj is the input minus `op` |
| `channel.publish/subscribe(obj)` | Channel | multi-op meta-tool; subscribe is a non-blocking peek |
| `agent.spawn(obj)` | Agent | spawn an LLM (or code) sub-agent; returns its result |
| `WebFetch(obj)` / `Read(obj)` / `HTTP(obj)` / `WebSearch(obj)` / … | the built-in of that name | every other allowed **built-in**, flat by canonical name |
| `mcp__<server>__<tool>(obj)` | that MCP tool | every allowed MCP tool, flat by name |

> **Naming:** `allowed_tools` uses loomcycle's **canonical** tool names
> (capitalized: `WebFetch`, `Memory`, `Read`, …), the same as LLM agents.
> The three multi-op meta-tools are exposed in JS as **lowercase objects**
> (`memory`, `channel`, `agent`); every other tool is a **flat function by
> its exact canonical name** (`WebFetch({url})`, `mcp__jobs__ingestJobs({…})`).
> Putting `memory` (lowercase) in `allowed_tools` silently omits the tool —
> your JS then sees `ReferenceError`. `memory.*` additionally needs
> `memory_scopes` declared (`[agent]` / `[user]`), and `WebFetch`/`HTTP`
> obey the agent's `allowed_hosts`, exactly as for LLM agents.

**Return types.** `memory` / `channel` / `agent` and `mcp__*` tools return
**parsed values** — their results are structured JSON, so
`memory.get(...).value` and `mcp__jobs__getContext(...).foo` just work. The
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

## Honest determinism

The promise is **"no LLM-induced non-determinism"**, not "perfectly
reproducible." Your JS can still call `Date.now()`, `Math.random()`, and
MCP tools whose upstream responses vary. The win is real anyway: zero
tokens, no model latency, no hallucination. For replay/testing,
`LOOMCYCLE_CODE_AGENTS_DETERMINISTIC=1` seeds `Date.now()` (fixed epoch)
and `Math.random()` (seeded PRNG) — but MCP responses stay whatever the
upstream returns.

## Sharp edges

- **Glue-logic fast, not data-processing fast.** goja is interpreted (no
  JIT). CPU-bound work over megabytes belongs in an MCP server. The
  design center is the ~100ms-budget glue step.
- **One tool call at a time.** Parallel tool calls within one code-agent
  are out of v1 (the suspend point is one-at-a-time). Fan out with
  `agent.spawn(...)`, which is already concurrent at the loomcycle layer.
- **Not resumable across a restart.** A code-agent's run state is live
  in-process JS; it cannot be resumed from a persisted transcript or in a
  different replica. A run cut off mid-flight fails loud
  (`code_agent_continuation_lost`) rather than silently re-running.
- **Run timeout is the universal cancel.** A parked tool call is cut by
  the ctx deadline, not by interrupting the interpreter; a CPU-bound loop
  is cut by both. Set `LOOMCYCLE_CODE_AGENTS_RUN_TIMEOUT_SECONDS` to bound
  wall time. The heap limit is best-effort (goja exposes no hard cap).
- **ABI versioning.** The JS-side API is versioned on its own semver
  (currently 1.0.0), separate from loomcycle's release vector. Breaking a
  signature is a major bump with a deprecation window.
