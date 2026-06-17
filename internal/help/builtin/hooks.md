---
name: hooks
description: Tool-use hooks — register HTTP webhooks that wrap tool dispatch. Pre-hooks rewrite/deny/widen a tool call before it runs; Post-hooks rewrite the result. Selectors by (agent, tool, phase), fail-open vs fail-closed, opt-in per-call host-widening, DB-backed in cluster mode.
---

# Tool-use hooks

A tool-use hook is an **operator- or app-registered HTTP webhook that the
agent loop calls around every matching tool dispatch**. A `pre` hook runs
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
  "phase": "pre",                  // "pre" | "post"
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
result, or an empty body passes it through.

When several hooks match, `pre` hooks run **earliest-registration-first**
and `post` hooks run **LIFO** (classic middleware nesting), ordered by
registration time.

## Fail-open vs fail-closed

`fail_mode` decides what a webhook timeout / 5xx / network error means:

- **`open`** (default) — the original input or result passes through
  unchanged. Right for telemetry-shaped hooks: a down hook must never
  block tool dispatch.
- **`closed`** — the tool call fails with `is_error=true`. Right for
  security-shaped hooks (an injection or DLP scanner) where a down hook
  letting payloads through would be the bug.

## Per-call host-widening (the one audited exception)

By default a hook can only **narrow** a call — it cannot reach past the
operator's static `allowed_hosts`/`allowed_tools` floor (CLAUDE.md trust
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
