---
name: dynamic-mcp
description: Register HTTP / Streamable-HTTP MCP servers at runtime via the MCPServerDef substrate.
---
Static `mcp_servers:` declarations in `loomcycle.yaml` fix the set of
upstream MCP servers at boot time. The v0.9.x **MCPServerDef**
substrate adds the runtime-mutable side: an operator (or an external
orchestrator authenticating with the loomcycle bearer) registers,
promotes, or retires an HTTP / Streamable-HTTP MCP server without a
yaml edit and without a restart. Once registered, every agent's
existing `mcp__<name>__<tool>` fallthrough resolver picks the new
tools up on first call.

The motivating use case is **n8n's MCP Server Trigger**: an n8n
workflow exposes its outbound integrations (Mailgun, HubSpot, Stripe,
the 400+ n8n nodes) as MCP tools at `https://your-n8n/mcp/<token>`.
Without dynamic registration, every n8n workflow change requires a
yaml edit + loomcycle restart. With it, the operator (or the n8n
workflow itself, via the loomcycle MCP server's `mcpserverdef`
meta-tool) registers itself and the new tools become callable
immediately.

## What dynamic registration is NOT

Read this section before anything else.

- **NOT exposed to agents.** The `MCPServerDef` tool is auto-attached
  to **zero** agents' `allowed_tools`. There is no per-agent
  substrate surface for it (deliberate departure from AgentDef /
  SkillDef, which agents CAN use to fork themselves). Only operators
  reach it — via `POST /v1/_mcpserverdef`, the loomcycle MCP
  meta-tool, the gRPC RPC, or the TS adapter — all bearer-authed
  against `LOOMCYCLE_AUTH_TOKEN`.
- **NOT stdio.** Stdio MCP servers stay yaml-only. Dynamic
  registration is HTTP + Streamable-HTTP only. The substrate refuses
  any other transport. The decision closes the agent-process-spawn
  escalation path entirely: there is no way to register a
  loomcycle-spawned subprocess at runtime.
- **NOT a yaml replacement.** Static and dynamic registrations
  coexist. A dynamic `create` colliding with a name already present
  in `cfg.MCPServers` is refused with a typed error — yaml is ground
  truth, dynamic rows live alongside.
- **NOT free-form URL.** The URL's hostname must appear in the
  operator's existing `LOOMCYCLE_HTTP_HOST_ALLOWLIST`. SSRF defence
  at the registration boundary; same allowlist that gates the
  `HTTP` and `WebFetch` tools.

## What gets persisted

A row in `mcp_server_defs` carries the substrate's content fields:

```
name, description, transport ("http" | "streamable-http"),
url, headers (map<string,string>), discovered_tools (cached;
refreshed via rediscover)
```

Plus the standard substrate metadata: `def_id`, `version`,
`parent_def_id`, `created_at`, `created_by_*`, `retired`,
`bootstrapped_from_static`, `content_sha256` (see
`help(topic="content-signatures")`).

Headers carry the same `${LOOMCYCLE_*}` + `${run.user_bearer:-FALLBACK}`
substitution patterns as static `mcp_servers.*.headers`. The
substitution happens per-request in `Client.do()` against a local map
copy — `c.headers` is never mutated, so concurrent calls send the
right bearer per run.

## Ops

The `MCPServerDef` tool dispatches eight ops:

| Op | What it does |
|---|---|
| `create` | New `(name, v1)` row + optional `promote: true` to make it active. Validates transport + URL hostname. Refuses name collisions with `cfg.MCPServers`. |
| `fork` | New `(name, v_next)` row built from `parent_def_id`'s overlay + caller's overlay. Same validation as `create`. |
| `get` | Fetch one row by `def_id` (or by `name` to get the active row). |
| `list` | Per-name summary (`version_count`, `latest_version`, `active_def_id`, `last_updated`). |
| `promote` | Swap the `mcp_server_def_active` overlay to a specific `def_id`. Evicts the old client from the MCP pool so the next call picks up the new URL / headers. |
| `retire` | Mark the active row retired, drop the active-overlay pointer, evict the pool entry. Reversible — `fork` from the retired def + `promote` brings it back. |
| `rediscover` | Re-runs `tools/list` against the upstream and refreshes the row's `discovered_tools` cache. Subject to a 30-second deadline. |
| `verify` | Caller-supplied `content_sha256` → `matches` / `current_sha256` / `deployed`. Mirrors AgentDef/SkillDef `verify`. |

## How the dispatcher picks up the new tools

The clever bit. Loomcycle's tool dispatcher's `allTools` slice is
frozen at agent-load time — it does NOT mutate. Yet a tool registered
via dynamic substrate must become callable without a restart.

The mechanism is the v0.8.1 **lazy MCP resolver**:

1. Agent emits `mcp__n8n-mailgun__sendMail`. Dispatcher's frozen
   `allTools` doesn't contain it.
2. Dispatcher falls through to `mcpLazyResolver.Resolve`.
3. Resolver calls `pool.Get("n8n-mailgun")`.
4. Pool's `build()` callback looks the name up in BOTH:
   - the yaml-loaded static map (`cfg.MCPServers`), AND
   - the in-process `DynamicRegistry`.
5. Hits the `DynamicRegistry` entry, builds the client, runs
   `tools/list` to populate the cached tool set, dispatches the call.
6. Subsequent calls hit the cached pool entry — same fast path as a
   static yaml entry.

There is no dispatcher refresh, no `allTools` mutation API, no
restart. The lazy-resolver pattern was designed for exactly this
case: "tools you didn't have at boot become callable at runtime."

`promote` evicts the pool entry so the next call rebuilds against
the new active row (different URL / headers). `retire` evicts the
pool entry; in-flight calls complete, new calls get a clean
"tool not found" error.

## Operator workflow

The shape is the same across the CLI, the TS adapter, the gRPC
surface, and the MCP meta-tool. The HTTP admin endpoint is the
simplest illustration:

```bash
# 1. Register a new dynamic MCP server (active immediately)
curl -X POST http://127.0.0.1:8787/v1/_mcpserverdef \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{
    "op": "create",
    "name": "n8n-mailgun",
    "overlay": {
      "transport": "streamable-http",
      "url": "https://your-n8n.example.com/mcp/abc123",
      "headers": {"Authorization": "Bearer ${LOOMCYCLE_N8N_TOKEN}"},
      "description": "n8n workflow exposing Mailgun send"
    },
    "promote": true
  }'

# 2. Inspect what's registered
curl http://127.0.0.1:8787/v1/_mcpserverdef \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op": "list"}'

# 3. Refresh the cached tool list after upstream adds new nodes
curl http://127.0.0.1:8787/v1/_mcpserverdef \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op": "rediscover", "name": "n8n-mailgun"}'

# 4. Retire when the n8n workflow is decommissioned
curl http://127.0.0.1:8787/v1/_mcpserverdef \
  -H "Authorization: Bearer $LOOMCYCLE_AUTH_TOKEN" \
  -d '{"op": "retire", "name": "n8n-mailgun"}'
```

## Self-registration from n8n (the load-bearing pattern)

The n8n workflow registers itself once at deploy time, then exposes
its tools indefinitely:

1. The n8n workflow's first node calls the loomcycle MCP server's
   `mcpserverdef` meta-tool with `op: create`, its own MCP endpoint
   URL, and the bearer token n8n generated.
2. Loomcycle persists the row + makes it active + the URL becomes
   callable from any agent.
3. If the n8n workflow is later updated (new nodes added), the
   workflow's deploy step calls `op: rediscover` to refresh
   loomcycle's cached tool list.
4. If the n8n workflow is decommissioned, its teardown step calls
   `op: retire`.

The pattern keeps the operator-toil at zero: the n8n side owns its
own lifecycle, the loomcycle side stays domain-agnostic.

## Snapshot round-trip

The `mcp_server_defs` + `mcp_server_def_active` tables ship as two
new snapshot sections in v0.9.x. A snapshot captured on a runtime
with N dynamic registrations restores them all into a target runtime
verbatim — the target's `DynamicRegistry` picks them up via the same
boot-time loader that runs on every start.

Section version "1.0", additive. Pre-v0.9.x readers find the sections
empty; pre-v0.9.x snapshots restored to v0.9.x readers leave the new
tables untouched.

## Errors at the registration boundary

| Error code | When |
|---|---|
| `mcp_server_def_transport_invalid` | `transport` is not `http` or `streamable-http`. |
| `mcp_server_def_url_invalid` | `url` is empty, malformed, or not http(s). |
| `mcp_server_def_host_not_allowed` | URL hostname isn't in `LOOMCYCLE_HTTP_HOST_ALLOWLIST`. |
| `mcp_server_def_name_collision` | A yaml `mcp_servers.<name>` already owns this name. |
| `mcp_server_def_name_invalid` | Name fails the substrate's `[a-z0-9-]{1,64}` shape. |
| `mcp_server_def_parent_not_found` | `fork` was passed a `parent_def_id` that doesn't exist. |
| `mcp_server_def_rediscover_failed` | The upstream `tools/list` exchange timed out or returned a protocol error. |

The errors are typed in the substrate response payload; consumers
discriminate by code rather than parsing the message string.

## References

- `help(topic="loomcycle")` — the runtime + tool model these
  registrations plug into.
- `help(topic="n8n-integration")` — the three composition patterns
  (loomcycle as MCP server, n8n as MCP server, planned community
  node); this topic is the operator's dynamic side of pattern #2.
- `help(topic="content-signatures")` — `content_sha256` shape +
  bundle-vs-deployed comparison workflow.
- `help(topic="skills-evolution")` — the SkillDef substrate this
  one's surface mirrors.
