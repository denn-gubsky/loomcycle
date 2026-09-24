---
name: hooks
description: Tool-use and run hooks — register a webhook or a JavaScript body that wraps tool dispatch or a run's start and finish. Pre-hooks rewrite/deny/widen a tool call before it runs; post-hooks rewrite the result or add context; agent_start may deny a run or add to its prompt; agent_stop may send an answer back or hold it for a person; a code hook can ask an operator and decide on the answer. Selectors by (agent, tool, phase), fail-open vs fail-closed, opt-in per-call host-widening, DB-backed in cluster mode.
---

# Tool-use hooks

A tool-use hook is an **operator- or app-registered HTTP webhook, or a
JavaScript body (see *Code hooks* below), that the agent loop calls around
every matching tool dispatch**. A `pre` hook runs
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
  shared runtime. Hooks keep that policy in the registering app, reachable
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

## Registering a hook

Hooks are registered dynamically over the bearer-authed admin surface:

```
POST   /v1/hooks          # register (returns {id})
GET    /v1/hooks          # list
DELETE /v1/hooks/{id}     # remove
```

A registration body:

```json
{
  "owner": "dlp-scanner",          // app UID; (owner, name) is the identity
  "name": "scan-web-fetches",
  "phase": "pre",                  // a tool phase: "pre" | "post" | "post_failure"; or a run phase (below)
  "agents": ["researcher", "qa-*"], // exact or "prefix*"; omit = match all
  "tools": ["WebFetch", "mcp__jobs__*"],
  "callback_url": "https://dlp.internal/loomcycle-hook",
  "fail_mode": "closed",          // "open" (default) | "closed"
  "timeout_ms": 800
}
```

`(owner, name)` is the identity: **re-registering the same pair replaces
the prior registration**, so an app restart re-announcing its hooks never
cascades duplicates. Selectors use a deliberately simple glob — exact
match or a single trailing `*` prefix (`mcp__jobs__*`); no regex, no
middle wildcards. An empty/omitted `agents` or `tools` list means "match
all". A hook fires only when its agent glob AND its tool glob AND its
phase all match.

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

When several hooks match, `pre` hooks run **earliest-registration-first**
and `post` hooks run **LIFO** (classic middleware nesting), ordered by
registration time. A tenant operator's hooks always run **before** the
operator's global ones, whenever each was registered, so the operator's hooks
have the last word: a tenant `pre` hook cannot rewrite an input after the
operator's hook approved it, and a tenant `post` or `post_failure` hook cannot
rewrite a result after the operator's hook checked it. The same holds for the
run hooks below.

## Run hooks

These phases are about the run rather than a tool call. They are selected by
`agents` only — the agent of the run they fire in; a `tools` selector is
refused.

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
through the Agent tool (one-shot or `parallel_spawn`); `agents` selects the
parent, and the payload names the child in `subagent`. `deny` refuses the child
— the parent's Agent call gets the reason as an error, and the child is never
created; `additional_context` is added to the child's prompt.

**`subagent_stop`** runs in the parent after the child finished, before its
result reaches the parent. The payload adds `subagent_run_id`, `status`
(`completed` / `failed`), `final_text` and `error`. `deny` refuses the result:
the parent gets the reason as an error and may try again. `additional_context`
is appended to the result. To send a child back to revise, register an
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

Instead of `callback_url`, a registration may carry `code`: JavaScript that
loomcycle runs in-process, with no network round-trip and no tokens. The
operator enables it with `LOOMCYCLE_CODE_HOOKS_ENABLED=1`; otherwise a code
registration is refused. Set exactly one of `callback_url` and `code`.

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
  where any other is. The hook asks under its own grant, so it works whether
  or not the agent itself may interrupt.
- `Interruption.notify({message})` informs without waiting.

Each ask runs the body again from the start, replaying the answers it
already has. So a body must decide only from `ev` and those answers, and a
global it sets does not survive to the next call. `timeout_ms` bounds each
run of the code: 50 ms by default, at most 1 s. The time an operator takes
to answer is not counted. A body may ask at most 16 times per call.

## Fail-open vs fail-closed

`fail_mode` decides what a webhook timeout / 5xx / network error means:

- **`open`** (default) — the original input or result passes through
  unchanged. A run that is cancelled while a hook is deciding still never
  runs the tool. Right for telemetry-shaped hooks: a down hook must never
  block tool dispatch.
- **`closed`** — the tool call fails with `is_error=true`. Right for
  security-shaped hooks (an injection or DLP scanner) where a down hook
  letting payloads through would be the bug.

## Per-call host-widening (the one audited exception)

By default a hook can only **narrow** a call — it cannot reach past the
operator's static `allowed_hosts`/`tools` floor (CLAUDE.md trust
rule). The single exception is a `pre` hook's `allow_hosts`, and it is
**off unless the operator opts the hook's owner in**:

```yaml
hooks:
  permit_host_widen:
    # entries are "[tenant:]owner" — exact match on both, no globs.
    owners: [acme:url-reputation-gate]   # owner "url-reputation-gate" in tenant "acme"
# or: LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS=acme:url-reputation-gate (env appends)
```

Each entry names a **(tenant, owner)** pair. A bare `owner` (no colon) binds
to the shared tenant `""` — the right form for a single-tenant deployment or
an operator/global hook; `tenant:owner` confines the grant to that tenant. The
key is `(tenant, owner)` and **not owner alone** because a hook's `owner` is a
caller-supplied string while only its tenant is authoritative — so without the
tenant a second tenant could register a hook with a permitted owner name and
widen hosts for its own runs, escaping the operator floor.

Only for a listed `(tenant, owner)` does the dispatcher union that hook's
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
- In **multi-replica** mode the registry is DB-backed (the `hooks` table,
  Postgres) with backplane cache-invalidation, so a hook registered on one
  replica fires for runs on any replica; the hot-path match stays in-memory
  cached and never hits the DB. (SQLite is single-replica only.)

**Bottom line:** tool-use hooks are the seam for wrapping tool dispatch
with an external app's policy or observability — narrowing by default,
fail-open or fail-closed by choice, with host-widening the one explicitly
operator-gated, audited way to reach past the static floor.
