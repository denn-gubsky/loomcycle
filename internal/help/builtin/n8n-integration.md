---
name: n8n-integration
description: Three patterns for composing loomcycle with n8n — bidirectional MCP plus the planned community node.
---
**n8n** (https://n8n.io) is a self-hostable workflow automation
platform — visual drag-and-drop node graph, 400+ integrations
(SaaS APIs, databases, messaging), and a built-in AI Agent node
that can call external tools over MCP. Loomcycle and n8n are
**architecturally complementary**: n8n is the visual designer
where you compose agentic systems; loomcycle is the substrate
where those systems actually execute.

The two integrate cleanly because **both support MCP
bidirectionally**: each can be the MCP server, each can be the
MCP client. Three integration patterns fall out of that, and
they're not mutually exclusive — a single deployment can use
all three for different parts of the system.

## Quick decision matrix

| You want… | Use Pattern |
|---|---|
| Use n8n's visual builder; have its AI Agent reach into loomcycle for memory / channels / sub-agent spawn / AgentDef versioning | **Pattern 1** — n8n consumes loomcycle's MCP |
| Use loomcycle as the agent runtime; have agents call n8n workflows that wrap Mailgun / Slack / Notion / GitHub / etc. | **Pattern 2** — loomcycle consumes n8n's MCP |
| Drag-and-drop loomcycle nodes inside n8n's visual builder (Spawn Run, Memory ops, Channel pub/sub, AgentDef get/fork) | **Pattern 3** — `n8n-nodes-loomcycle` community node (planned) |

Patterns 1 and 2 work TODAY against the existing loomcycle wire
surface — no new dependencies, no plugins. Pattern 3 is the
upcoming `denn-gubsky/n8n-nodes-loomcycle` npm package; until it
ships, Patterns 1 + 2 cover the same surface with two more
configuration steps.

## Pattern 1 — n8n's AI Agent calls loomcycle via MCP

Use this when n8n is your control surface and loomcycle provides
the substrate features n8n's built-in AI Agent doesn't cover
(persistent Memory, durable Channels, AgentDef versioning,
sub-agent spawning, Evaluation aggregates).

**Setup:**

1. Run loomcycle with the MCP server enabled. The `loomcycle mcp`
   subcommand starts both the HTTP listener AND a stdio MCP
   listener; the HTTP MCP transport at `POST /v1/_mcp` is
   available whenever the HTTP listener is up. Either transport
   works.
2. In n8n's AI Agent node, add an **MCP Client Tool** sub-node.
3. Configure the MCP server URL or command to point at loomcycle.
4. n8n auto-discovers loomcycle's 28 meta-tools (`memory`,
   `channel`, `agentdef`, `skilldef`, `evaluation`, `context`,
   `interruption_resolve`, `spawn_run`, `cancel_run`, `get_run`,
   `list_runs`, `register_agent`, `unregister_agent`,
   `list_agents`, `register_hook`, `list_hooks`, `delete_hook`,
   `pause_runtime`, `resume_runtime`, `get_runtime_state`,
   `create_snapshot` / `list_snapshots` / `get_snapshot` /
   `export_snapshot` / `restore_snapshot` / `delete_snapshot`,
   `list_channels`, `stream_user_run_states`).
5. The AI Agent can now call any of them like any other tool.

**Concrete example:** an n8n AI Agent that uses loomcycle's
Memory as persistent state across workflow invocations. The
agent calls `memory_set` during one run, `memory_get` during
another; state survives between n8n executions and is also
visible to loomcycle-side agents in the same scope.

## Pattern 2 — loomcycle agents call n8n workflows as MCP tools

Use this when loomcycle is your control surface and you want
loomcycle agents to reach n8n's 400+ integrations (Mailgun for
email, Slack for chat, Notion for docs, GitHub for issues,
Stripe for billing, …) without writing a per-integration tool
in Go.

This is the **inverse-MCP pattern**: n8n exposes its workflow
as an MCP server using its built-in **MCP Server Trigger** node;
loomcycle consumes that MCP server like any other MCP upstream.

**Setup:**

1. In n8n, build a workflow that does the integration you want
   — for example, a workflow that takes `{to, subject, body,
   attachments?}` and sends an email via Mailgun.
2. Use n8n's **MCP Server Trigger** as the entry node, exposing
   the workflow as an MCP server endpoint. n8n shows the URL +
   the bearer token it minted for the trigger.
3. In your `loomcycle.yaml`, declare the n8n workflow as an
   upstream MCP server:

   ```yaml
   mcp_servers:
     n8n-mailgun:
       transport: http
       url: https://your-n8n.example.com/mcp/abc123
       headers:
         Authorization: "Bearer ${LOOMCYCLE_N8N_MCP_TOKEN}"
   ```

   The env-var name MUST be `LOOMCYCLE_*`-prefixed (or on the
   documented third-party allowlist) for `${...}` expansion to
   work. See the operator-yaml notes in `loomcycle.example.yaml`
   for the full allowlist.
4. Set `LOOMCYCLE_N8N_MCP_TOKEN` in `.env.local` with the
   bearer n8n minted.
5. Grant the agent permission to call the tool — its
   `allowed_tools` block lists `mcp__n8n-mailgun__send` (or
   whatever your workflow's MCP Server Trigger names the
   operation).

Loomcycle now treats the n8n workflow as a typed tool. When the
agent calls it, loomcycle forwards via the existing MCP HTTP
client (the same code path used for `jobs-search-agent` /
`brave-search` / any other MCP upstream). n8n executes the
workflow, hits Mailgun, returns the result.

**Why this is "the 400+ integrations finding":** building those
integrations as loomcycle built-ins would be consumer-product
work that violates loomcycle's substrate stance. Building them
in n8n is what n8n is for. The MCP bridge makes the boundary
clean — loomcycle stays small; the ecosystem lives in n8n.

### Per-user bearer with ${run.user_bearer}

For per-end-user authentication (each agent run authenticates
to n8n as the actual end-user, not as a static service account),
use loomcycle's v0.8.14 per-run bearer substitution:

```yaml
mcp_servers:
  n8n-user-scoped:
    transport: http
    url: https://your-n8n.example.com/mcp/per-user
    headers:
      Authorization: "Bearer ${run.user_bearer:-${LOOMCYCLE_N8N_FALLBACK_TOKEN}}"
```

Each `POST /v1/runs` carrying a `user_bearer` field gets its
own outbound Authorization header. The fallback (after `:-`)
catches runs without a user_bearer (cron jobs, internal
operators) so the workflow still works.

## Pattern 3 — n8n-nodes-loomcycle community node (planned)

Use this when you want first-class drag-and-drop loomcycle
nodes inside n8n's visual builder, with native UI for spawning
runs, reading/writing Memory, publishing Channels, AgentDef
ops, Evaluation submissions, etc.

**Status:** Phase 2 of the n8n integration RFC. Separate npm
package + separate repo (`denn-gubsky/n8n-nodes-loomcycle`),
not yet published. Ships ~2-3 weeks after the Phase 0 wire-API
additions (the `GET /v1/_channels` + `GET /v1/users/{user_id}/agents/stream`
endpoints in this loomcycle release; the community node
depends on both for the auto-discovery + trigger plumbing).

**Planned surface:**

- **Action nodes** — `LoomCycle: Run` (Spawn / Get Status /
  Wait / Cancel / List Agents), `LoomCycle: Memory` (Get / Set /
  Increment / List / Delete / Sweep / Search), `LoomCycle:
  Channel` (Publish / Subscribe / Peek / Ack / List Channels),
  `LoomCycle: AgentDef`, `LoomCycle: SkillDef`, `LoomCycle:
  Evaluation`, `LoomCycle: Context`.
- **Trigger nodes** — `LoomCycle: Run Completed` (fires on
  terminal run states via the new SSE endpoint), `LoomCycle:
  Channel Message` (fires on new channel messages).
- **Cluster sub-nodes** — `LoomCycle Memory Tool`, `LoomCycle
  Channel Tool`, `LoomCycle Sub-Agent Tool`, plug into n8n's
  AI Agent as drag-and-drop tools.
- **Auto-discovery** — the credential picker queries
  `GET /v1/_channels` and `GET /v1/users/{user_id}/agents` to
  populate dropdowns for channel names + agent names.

Until the community node ships, Patterns 1 + 2 cover the same
operational surface — what you give up is the visual UX.

## Streaming run state (the new endpoint Phase 0 ships)

Both Pattern 3 trigger nodes use the same new endpoint operator
dashboards can also subscribe to:

```
GET /v1/users/{user_id}/agents/stream?status=completed,failed
Authorization: Bearer ${LOOMCYCLE_AUTH_TOKEN}
Accept: text/event-stream
```

SSE frames:

```
event: stream_open
data: {"user_id":"u_42","filter_status":["completed","failed"],"filter_agent":"","keepalive_interval":25}

event: run_state
data: {"run_id":"r_abc","agent_id":"ag_xyz","agent":"researcher","user_id":"u_42","status":"running","ts":"..."}

event: run_state
data: {"run_id":"r_abc","status":"completed","stop_reason":"end_turn","ts":"..."}
```

Streams stay open for up to 30 minutes; 25s keepalive comment
frames keep reverse proxies (nginx default, cloudflare free,
n8n's idle timeout) from dropping the connection. The TS
adapter exposes this as
`client.streamUserRunStates(userId, opts)` returning an
`AsyncIterable`; the gRPC surface offers a server-streamed
`StreamUserRunStates` RPC; the MCP meta-tool
`stream_user_run_states` exposes both a blocking aggregate mode
and (when the session opted into runEvents=true) per-event
`notifications/loomcycle/run_state` notifications.

## Combining patterns

A single deployment can use all three concurrently:

- **Pattern 3** for the bulk of operator-facing loomcycle ops
  (drag-and-drop nodes in n8n).
- **Pattern 1's MCP Client Tool** inside an AI Agent for ad-hoc
  loomcycle tool calls the community node doesn't cover yet.
- **Pattern 2's MCP Server Trigger** to expose its own custom
  n8n workflows back to loomcycle agents as tools.

Pick what fits each part of the system. The patterns are
substrate-neutral — none of them lock you in.

## What this composition deliberately does NOT do

- It does NOT pull n8n features into loomcycle's Web UI.
  Loomcycle's `/ui` stays read-only monitoring; visual workflow
  design lives in n8n.
- It does NOT reimplement n8n's 400+ integrations as loomcycle
  built-in tools. The whole point of Pattern 2 is that
  loomcycle DOESN'T need its own integration library.
- It does NOT add a cron scheduler to loomcycle for n8n-
  triggered workflows. n8n already has cron; use it. Loomcycle's
  deferred-publish on Channels (`deliver_at`) is for inter-agent
  timing, not workflow scheduling.
- It does NOT replace n8n's built-in AI Agent. Both have
  legitimate use cases. Simple flows: n8n's AI Agent. Complex /
  multi-tenant / self-evolving / audit-grade: loomcycle.

## References

- `help(topic="loomcycle")` — the substrate overview, what each
  built-in does, when to spawn vs publish.
- `help(topic="system-channels")` — the `_system/*` prefix and
  the admin-publish endpoint Pattern 2 / 3 workflows can post to.
- `help(topic="subagents")` — Agent-tool semantics that
  n8n-spawned loomcycle agents inherit.
- `loomcycle.example.yaml` — the commented `mcp_servers.n8n-*`
  block as a starting point for Pattern 2.
- n8n docs: https://docs.n8n.io/integrations/builtin/cluster-nodes/sub-nodes/n8n-nodes-langchain.toolmcpclient/
  (MCP Client Tool) and https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-langchain.mcptrigger/
  (MCP Server Trigger).
