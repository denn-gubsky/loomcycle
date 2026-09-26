---
name: hooks
description: Tool-use and run hooks — webhooks or JavaScript bodies an agent's definition carries, per tool and per agent, that wrap its tool calls and its run's start and finish. Pre-hooks rewrite/deny/widen a tool call before it runs; post-hooks rewrite the result or add context; agent_start may deny a run or add to its prompt; agent_stop may send an answer back or hold it for a person; a code hook can ask an operator and decide on the answer. Attached in the AgentDef (inline webhooks or HookDefs), fail-open vs fail-closed, opt-in per-call host-widening.
---

# Tool-use hooks

A tool-use hook is an **HTTP webhook, or a JavaScript body (see *Code hooks*
below), that an agent's definition attaches and the agent loop calls around
its matching tool calls**. A `pre` hook runs
*before* the tool, and can rewrite the input the tool sees, deny the call
with a synthetic result the model receives instead, or (when explicitly
permitted) widen the host allowlist for that one call. A `post` hook runs
*after* the tool and can rewrite the result before the model sees it.

## Why a webhook seam, not a code change

The obvious alternatives are worse for the problem hooks solve — injecting
**cross-cutting policy or observability around tool calls from a system
loomcycle doesn't own**:

- **Editing the agent or forking loomcycle** couples one app's policy
  (an injection scanner, a DLP filter, a per-tenant audit log) into the
  shared runtime. Hooks keep that policy in the app that serves the webhook, reachable
  over HTTP, with no loomcycle redeploy.
- **A man-in-the-middle proxy** in front of each MCP server sees the wire
  call but not the loomcycle context (which agent, which run, which user)
  and can't short-circuit with a model-visible synthetic result.

A hook sees `(agent, tool, input)` plus correlation IDs, runs at the exact
dispatch boundary, and composes with the operator's static policy instead
of replacing it.

This is distinct from two things it's easy to confuse it with:

- **`on_complete` schedule hooks** (see `scheduled-runs`) fire *after a
  whole run* and deliver to `channel.publish` / `memory.set` / `mcp.call`.
  Tool-use hooks fire *around each tool call*. Different feature, different
  config.
- **MCP tools / the LocalAPI gateway** *add* capabilities to an agent.
  Hooks *wrap* the capabilities it already has.

## Attaching hooks to an agent

A run carries its hooks: it takes them **verbatim from its agent's
definition** when it starts, and fires exactly those. There is no global
registration — an agent with no hooks runs with none, and one agent's hooks
never fire on another agent's run.

A tool's hooks belong to that tool's entry in `tools`; the agent's own `hooks`
hold the run events and tool events for every tool:

```yaml
agents:
  researcher:
    tools:
      - Read
      - name: WebFetch                       # this tool's own hooks
        hooks:
          pre:
            - deny-internal                  # a HookDef (the active version)
            - { name: url-gate, url: "https://app.example/hooks/url-gate", fail_mode: closed, timeout_ms: 800 }
          post:
            - { name: content-scrubber, url: "https://app.example/hooks/scrub", fail_mode: closed }
    hooks:                                   # the agent's own: run events, and every tool
      agent_stop: [cite-sources@3]           # a HookDef pinned to version 3
      post: [redact-secrets]
```

The same shape goes in an AgentDef overlay (`tools` entries as
`{name, hooks}`; stored as the tool name plus a `tool_hooks` entry).

- An entry is a **HookDef name** (`gate`, or `gate@3` pinned) or an **inline
  webhook** `{name, url, fail_mode, timeout_ms, headers}` — `name` required; it
  names the hook in the payload and in `hook_decision` events.
- **Secrets go in `headers`, as credentials** — never in the URL or a literal
  value, which would sit in the agent's definition for anyone who can read it.
  A header value may name a credential, `$cred:<name>` (see `credentials`),
  resolved for the run each time the hook is called:
  `headers: {Authorization: "Bearer $cred:hook_secret"}`. A credential that
  does not resolve for the run fails the call (never sent as the literal
  reference), and the hook's `fail_mode` decides. A HookDef's `http` body takes
  the same `headers`.
- A tool's hooks may use only `pre`, `post`, `post_failure`, and only for a
  tool the agent has; a HookDef under an event it does not answer is refused.
  These are checked when an AgentDef is saved, and again when a run starts.
- A HookDef reference resolves in the tenant that owns the definition, then in
  the shared tenant — never in the run's tenant, so a tenant cannot replace an
  operator's hook by naming its own the same.
- **A hook that cannot be resolved stops the run before any model call** (a
  deleted or retired HookDef, say): a gate the definition names must not
  silently be missing.
- A sub-agent fires its own definition's hooks, not its parent's.
- The payload's `owner` says where a hook came from (`agent:<name>`).

## Hook definitions (HookDef)

A **HookDef** is one hook stored once and versioned, so it can be named
wherever it is used instead of copied into each definition: the event it answers, the
tools it matches, its body, its fail mode and timeout.

```json
{ "op": "create", "name": "net/deny-internal",
  "overlay": {
    "description": "Blocks fetches of internal hosts.",
    "event": "pre",
    "match": { "tools": ["WebFetch", "HTTP"] },
    "body": { "kind": "http", "url": "https://hooks.example/gate" },
    "fail_mode": "closed",
    "timeout_ms": 800 } }
```

- `event` uses the phase names above (`pre`, `post`, `post_failure`,
  `agent_start`, `agent_stop`, `subagent_start`, `subagent_stop`,
  `pre_compact`, `post_compact`, `run_end`); `match.tools` is for the tool
  events only.
- `body.kind` is `http` (a `url`) or `code-js` (a `code` body defining
  `hook(ev)`, see *Code hooks*). A code body is compiled when it is saved, and
  is refused unless the server enables code hooks.
- `description` is what someone the hook stops is told about it; the body and
  URL are never shown to them.
- Ops: `create` (promotes), `fork` (each top-level overlay field replaces the
  parent's; `null` clears it; does not promote), `get` (by `def_id`, or `name`
  for the active version), `list`, `promote`, `retire`, `verify`, `delete`
  (every version of the name).
- Names are segments of `A-Z a-z 0-9 _ -` joined by `/`; they cannot contain
  `@` or `:`.
- A HookDef belongs to its tenant: another tenant's reads back as not found,
  and a fork's parent must be your own tenant's.

Surfaces: `POST /v1/_hookdef` (and `GET /v1/_hookdef/names`), the MCP tool
`hookdef`, the gRPC `HookDef` RPC, and the TS / Python adapters (`hookDef` /
`hook_def`). It is never an agent tool — no agent can write the hook that
gates it.

A HookDef on its own fires on nothing: it is attached by naming it from an
agent's definition (above).

## What a hook can return

A `pre` webhook response (`PreHookResult`) — any field may be set, and an
empty body / `204` passes the call through unchanged:

- `input` — the tool runs with this input instead of the model's.
- `deny` — the tool does **not** run; this `{text, is_error}` payload
  becomes the synthetic `tool_result` the model sees.
- `allow_hosts` — hostnames this hook approves for **this one call** (see
  host-widening below); opt-in and gated.

A `post` webhook response (`PostHookResult`): `result` replaces the tool
result, `additional_context` is appended to the result's text, or an empty
body passes it through. A `post` hook sees every result, failed or not; a
`post_failure` hook runs only when the tool failed, before the `post` chain,
with the failure's classification in `tool_result.error`.

When several hooks match, they run in the order the definition lists them — a
tool's own hooks before the agent-level ones — and `post` hooks run in reverse
(classic middleware nesting: the outer hook sees what the inner ones did). The
same order holds for the run hooks below.

## Run hooks

These phases are about the run rather than a tool call. They go under the
agent's own `hooks`, never under a tool.

**`agent_start`** runs once per run, after the prompt is composed and before
the first model call. A resumed run has already started, so it does not run
again. The payload is the run's identity (`agent`, `user_id`, `run_id`,
`parent_run_id`). The response:

- `{"decision": "deny", "reason": "..."}` — the run ends before any model
  call, failed with the reason;
- `{"additional_context": "..."}` — added to the prompt's user turn.

**`agent_stop`** runs each time the model finishes an answer, before the run
ends or waits for its next message. The payload adds `final_text`,
`stop_reason`, and — when the answer is a retry — `stop_hook_active` and
`stop_blocks`. The response:

- `{"decision": "block", "reason": "..."}` — the reason goes back to the model
  as a user turn and it answers again. A reason is required. More than 3 blocks
  in a row fail the run, naming the hook and its last reason, so a check that
  can never be satisfied cannot loop a run forever.
- `{"decision": "hold", "reason": "..."}` — the answer is held for a person's
  verdict, exactly as a run under review is held (`awaiting_review`, now with
  `held_by` naming the hook), and decided with the review verb: approve lets it
  through, reject with feedback sends it back, reject without ends it rejected.
  Disarming review does not release a hook's hold. A run nothing can deliver a
  verdict to (a sub-agent spawned by the Agent tool) ends rejected instead.
- nothing, or `{"decision": "allow"}` — the answer stands.

When several agent_stop hooks match, the first `block` wins and stops the
chain; a `hold` does not stop it, so a later hook may still block. A hook that
fails holds the answer if it is `fail_mode: closed`, and lets it through if
`open`. agent_stop hooks do not apply to a stateful run, whose product is its
state rather than an answer; the run says so.

**`subagent_start`** runs in the parent when it is about to start a child
through the Agent tool (one-shot or `parallel_spawn`); the hook is the
parent's, and the payload names the child in `subagent`. `deny` refuses the child
— the parent's Agent call gets the reason as an error, and the child is never
created; `additional_context` is added to the child's prompt.

**`subagent_stop`** runs in the parent after the child finished, before its
result reaches the parent. The payload adds `subagent_run_id`, `status`
(`completed` / `failed`), `final_text` and `error`. `deny` refuses the result:
the parent gets the reason as an error and may try again. `additional_context`
is appended to the result. To send a child back to revise, give the child an
`agent_stop` hook: it fires on the child's own run (its `parent_run_id` names
the parent).

**`pre_compact`** runs before a compaction summarizes the conversation.
`trigger` says what asked (`manual`, `auto`, `self`); `context_tokens` and
`window` the footprint. `deny` refuses it: a manual compaction
(`POST /v1/runs/{id}/compact`) gets a 409 with code `denied_by_hook`, before any
summary is made; an automatic one is recorded as a declined compaction with
reason `denied_by_hook`.

**`post_compact`** and **`run_end`** only report. `post_compact` gets
`trigger`, `before_tokens` and `after_tokens`; `run_end` gets `status`
(`completed` / `failed` / `cancelled` / `rejected`), `stop_reason`, `error` and
`final_text`, after the run's row is final. They run off the run's path, so
they never delay it, bounded at 30 s; their answer is ignored and a failure is
only logged. A run whose process died (marked failed by the stale-run sweep)
never reaches run_end.

A code hook returns the same shapes (`ev.event` is the phase name), and can
ask before deciding — an automated reviewer for the clear cases, a person for
the rest. A `post_compact` or `run_end` code hook may `notify` but not `ask`:
what it reports on has already happened.

## Code hooks

A HookDef's body may be `{"kind": "code-js", "code": ...}`: JavaScript that
loomcycle runs in-process, with no network round-trip and no tokens. The
operator enables it with `LOOMCYCLE_CODE_HOOKS_ENABLED=1`; otherwise a code
body is refused when the HookDef is saved. Tenant operators may write code
hooks too. (An inline hook in an agent's definition is always a webhook: code
lives in a HookDef, which is versioned and reviewable.)

```js
function hook(ev) {
  // ev: the payload a webhook would receive, plus ev.event —
  // "pre_tool_use", "post_tool_use" or "post_tool_use_failure".
  var url = ev.tool_call.input.url || "";
  if (ev.event === "pre_tool_use" && /\.internal\//.test(url)) {
    var a = Interruption.ask({question: "Let " + ev.agent + " fetch " + url + "?", options: ["allow", "deny"]});
    return a === "allow" ? {} : {decision: "deny", reason: "the operator declined"};
  }
  // Returning nothing lets the call through.
}
```

What the body returns:

- **pre:** `{decision: "deny", reason}` stops the call (the reason is what
  the model sees); `{updated_input}` rewrites the input; `{allow_hosts}` is
  the same gated per-call grant as a webhook's.
- **post / post_failure:** `{updated_output: {text, is_error}}` replaces the
  result; `{additional_context}` is appended to it.

A field that does not apply to the phase, or a misspelt field, counts as the
hook failing, and `fail_mode` decides — a mistake is reported, not ignored.

The body runs in the code-agent sandbox (no fetch, require, filesystem, eval
or Function; a deterministic clock and RNG), and its **only tool is
`Interruption`**:

- `Interruption.ask({question, options, context, timeout_ms})` holds the
  tool call until an operator answers. It returns the answer, or `null` if
  the operator declined; a timeout or cancellation throws, so an uncaught one
  fails the hook. The question is a pending interrupt on the run, answered
  where any other is — by the run's own user, who is the main actor: a hook
  gates the agent, not the person who started it. (An isolated member sees and
  answers only its own runs' questions.) The hook asks under its own grant, so
  it works whether or not the agent itself may interrupt.
- `Interruption.notify({message})` informs without waiting.

Each ask runs the body again from the start, replaying the answers it
already has. So a body must decide only from `ev` and those answers, and a
global it sets does not survive to the next call. `timeout_ms` bounds each
run of the code: 50 ms by default, at most 1 s. The time an operator takes
to answer is not counted. A body may ask at most 16 times per call.

The sandbox bounds a body's **time, not its memory**. The time budget is
checked between instructions, never inside one built-in call, so the one-call
allocators are capped: a string from `repeat` / `padStart` / `padEnd` at 1 Mi
characters, an array from `Array(n)` / `Array.from` — and `join` / `fill` on
one — at 65,536 elements, an `ArrayBuffer` or typed array at 1 MiB. Past a cap
the call throws a `RangeError`. These caps are a backstop, not a memory limit:
a body that grows a string in a loop is bounded only by its time budget.

## Fail-open vs fail-closed

`fail_mode` decides what a webhook timeout / 5xx / network error means:

- **`open`** (default) — the original input or result passes through
  unchanged. A run that is cancelled while a hook is deciding still never
  runs the tool. Right for telemetry-shaped hooks: a down hook must never
  block tool dispatch.
- **`closed`** — the tool call fails with `is_error=true`. Right for
  security-shaped hooks (an injection or DLP scanner) where a down hook
  letting payloads through would be the bug.

A security check must be `fail_mode: closed`. Under `open`, whatever makes the
hook fail skips the check — and the model controls the input: an oversized
input, or one slow to scan, can make the hook time out on purpose. This matters
most for a code hook, whose default time budget is 50 ms and whose own default
fail mode is `open` like any hook's.

## Per-call host-widening (the one audited exception)

By default a hook can only **narrow** a call — it cannot reach past the
operator's static `allowed_hosts`/`tools` floor (CLAUDE.md trust
rule). The single exception is a `pre` hook's `allow_hosts`, and it is
**off unless the operator opts the hook in**:

```yaml
hooks:
  permit_host_widen:
    # entries are "[tenant:]name" — the tenant that owns the definition the
    # hook came from, and the hook's name. Exact match on both, no globs.
    owners: [acme:url-gate]
# or: LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS=acme:url-gate (env appends)
```

A bare `name` binds to the shared tenant `""` — the operator's own yaml. And
the grant counts only when the hook came from an **operator-authored**
definition (the operator's yaml, or an AgentDef written under an operator
token): a hook an agent's own definition carries never widens hosts, whatever
the list says.

Only for such a hook does the dispatcher union that hook's
`allow_hosts` into a **ctx-scoped, this-call-only** extra list the
HTTP/WebFetch tools consult — no server-side cache, **not inherited by
sub-agents**. An un-permitted grant is dropped with a WARN
log + a `hooks_host_widen_total` metric. Matching is intentionally
stricter than the operator allowlist: a bare hostname is **exact-match**
(`acme.com` ≠ `careers.acme.com`); a leading-dot entry (`.acme.com`) is
suffix-match (host + subdomains).

**Confused-deputy hazard:** never derive `allow_hosts` blindly from the
tool input — the URL the model wants is untrusted. Validate independently
(user preference, per-tenant allowlist, reputation service). The
`host_widened` audit event exists so operators can spot abuse post-hoc.

## Trust model & caveats

- Hooks run **after** the policy layer. The worst a hook can do is
  short-circuit one tool call with a synthetic error; it cannot tear down
  the run or widen policy (except the opt-in host-widen above).
- Webhook payloads carry `agent_id` + `user_id` for correlation but
  **not** the agent's prompt or message history.
- A hook is on the **hot path** of every matching tool call — keep
  `timeout_ms` tight, and prefer `fail_mode: open` unless the hook is a
  security gate.
- A run's hooks are resolved once, when it starts, from its definition, so
  every replica fires the same set for a run; a HookDef promoted later does
  not change a run already going.

**Bottom line:** tool-use hooks are the seam for wrapping tool dispatch
with an external app's policy or observability — narrowing by default,
fail-open or fail-closed by choice, with host-widening the one explicitly
operator-gated, audited way to reach past the static floor.
