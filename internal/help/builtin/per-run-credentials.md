---
name: per-run-credentials
description: Per-tool named credentials map for runs (RFC F) — multi-MCP authorization, ${run.credentials.<name>} substitution, v0.8.x back-compat, sub-agent inheritance.
---
Loomcycle v1.x ships per-tool named credentials so a single run can
authenticate against multiple authorized MCP servers, each with its
own per-user token. The wire shape extends v0.8.14's single-bearer
`user_bearer` field with a `user_credentials: map<string, string>`
keyed by operator-chosen name. New `${run.credentials.<name>}`
substitution in `mcp_servers.*.headers` extends the existing
`${run.user_bearer}` mechanism with per-server indirection.

This is **off by default** in the sense that operators who haven't
written `${run.credentials.<name>}` into any `mcp_servers` config
see no behaviour change. Adding the substitution to an MCP server's
headers is the opt-in.

## Why multiple credentials per run

A scheduled agentic team typically fans out across several
authorized downstream services in one run:

- `mcp__jobs__postSearchIngest` writes results to JobEmber's DB —
  authorized by a JobEmber-issued per-user bearer
- `mcp__slack__send_message` posts a brief to the user's Slack DM —
  authorized by the user's Slack OAuth
- `mcp__telegram__send_dm` sends the same brief over Telegram —
  authorized by the bot token + chat ID

Each service has its own token. A single `user_bearer` covers
one of them; the others need their own credentials. The map shape
lets a single run carry all three transparently.

## The wire shape

`POST /v1/runs`, `POST /v1/sessions/{id}/messages`, gRPC `Run` and
`Continue`, MCP `spawn_run` tool, and the TS adapter `runStreaming`
all accept the same field:

```json
{
  "agent": "job-search-batch",
  "user_id": "alice@example.com",
  "user_credentials": {
    "jobs":     "<bearer-for-jobs-MCP>",
    "slack":    "xoxp-<token-for-slack-MCP>",
    "telegram": "<token-for-telegram-MCP>"
  },
  "segments": [...]
}
```

The map's keys are operator-chosen. The convention is to use the
same name as the MCP server's yaml key (so `${run.credentials.jobs}`
matches `mcp_servers.jobs`), but the substrate does not enforce
that — operators can use any naming taxonomy that suits them.

**Validation:** keys match `[a-zA-Z0-9_-]{1,64}`; values are
arbitrary strings; the empty map is valid (= run uses no per-tool
auth). Validation happens at the wire entry; the same regex is the
runtime-substitution regex, so a key that survives validation will
match the template if used.

## The substitution

In the operator's `loomcycle.yaml`:

```yaml
mcp_servers:
  jobs:
    transport: http
    url: https://api.jobember.com/mcp
    headers:
      Authorization: "Bearer ${run.credentials.jobs}"
  slack:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-slack"]
    env:
      SLACK_BOT_TOKEN: "${run.credentials.slack}"
```

**HTTP transport (canonical path):** substitution happens
per-request in `internal/tools/mcp/http/client.go` `Client.do()`.
The header expression `${run.credentials.jobs}` resolves against
the run's `RunIdentityValue.UserCredentials["jobs"]`. The substrate
never mutates the operator's static `mcp_servers.<name>.headers`
map; each outbound request constructs its own substituted copy.

**stdio transport (out of scope in v1.x):** stdio MCP servers are
spawned once per server at pool start and reuse the same child
process across all requests. Per-request env-var substitution would
require respawning per call (very expensive) or maintaining a
credential-keyed pool of server instances (significant complexity).
The current implementation leaves `${run.credentials.<name>}`
literals UNSUBSTITUTED in stdio env-var values — operators with
per-user auth needs against a stdio server have two options:

1. **Prefer HTTP transport** when the MCP server supports it. The
   canonical per-user auth path.
2. **Bake operator-env values** (`env_passthrough`,
   `env.SOMETHING_TOKEN: "${LOOMCYCLE_OPERATOR_TOKEN}"`) and ship
   one stdio server per operator-managed identity. Wasteful but
   the existing v0.8.x escape hatch.

A future RFC may extend stdio with a per-call substitution
mechanism; v1.x scopes that out.

## Two forms (strict + fallback)

Same shape as the v0.8.14 `${run.user_bearer}` syntax:

| Expression | Behaviour |
|---|---|
| `${run.credentials.jobs}` | Strict — empty/absent → header dropped |
| `${run.credentials.jobs:-FB}` | POSIX-style fallback to `FB` |

Bare expressions (no fallback) where the credential is missing
cause the entire header to be dropped from the outbound request.
A WARN log entry is emitted naming the missing key + the
`agent_id` so operators can triage. The upstream MCP server then
returns its own auth error (typically 401), which the loop
surfaces as a typed tool error — more debuggable than a
substrate-side dispatch failure.

## Back-compat with v0.8.14 `user_bearer`

The legacy `user_bearer: <string>` field on the wire stays valid
indefinitely. At `WithRunIdentity` time the substrate applies sugar:

- If `UserBearer` is non-empty AND `UserCredentials["default"]` is
  empty, the substrate promotes `UserBearer` to
  `UserCredentials["default"]` for the lifetime of the ctx.
- `${run.user_bearer}` substitution paths continue to use
  `UserBearer` directly (unchanged from v0.8.x).
- `${run.credentials.default}` also resolves correctly thanks to
  the promotion.

This means v0.8.x callers see zero behaviour change. A migration
to the new shape is purely additive — operators who want named
credentials add them; operators who don't keep working.

## Sub-agent inheritance

Sub-agents inherit the parent run's full credentials map via the
existing `RunIdentityValue` ctx propagation. The Agent built-in
tool's sub-agent spawn path carries the map identically — same
trust posture as the v0.8.14 `UserBearer` inheritance (sub-agents
act on behalf of the same end-user, so they get the same tokens).

There is no per-sub-agent credential narrowing in v1.x. If an
operator needs that, the recommended pattern is to spawn the sub-
agent via a separate `POST /v1/runs` call with a narrowed map.

## Trust + persistence posture

Credentials are NEVER persisted to:

- Run transcripts (`events` table) — same exclusion as
  v0.8.14 single-bearer
- OTEL spans (`internal/otel/recorder.go` posture — secrets stay
  out of trace attributes; `observability` help topic covers the
  full exclusion list)
- Snapshots (`pause-resume-snapshot` v0.8.17 format) — the
  credential map lives only in the runtime ctx + the outgoing
  request payloads
- Process logs — the WARN log on missing-credential emits ONLY the
  key name + `agent_id`, never values

The credential map lives in memory for the lifetime of the run's
ctx; cleared on run completion. The outgoing MCP request payloads
carry the substituted values (unavoidable), but loomcycle's own
persistent state never sees them.

## Worked example

JobEmber's on-demand search workflow:

```
1. Alice clicks "Search Jobs" in JobEmber's web app.

2. JobEmber's backend assembles:

   POST /v1/runs
   Authorization: Bearer <operator-token>
   {
     "agent":   "job-search-batch",
     "user_id": "alice@example.com",
     "user_credentials": {
       "jobs":     "<JobEmber-bearer-for-alice>",
       "slack":    "xoxp-<alice-slack-oauth>",
       "telegram": "<telegram-bot-token>"
     },
     "segments": [...]
   }

3. Server builds RunIdentityValue.UserCredentials from the map.
   ctx flows through to the agent loop.

4. job-search-batch agent fan-outs to job-searcher workers; each
   inherits the parent's credentials map via ctx.

5. A worker calls mcp__jobs__postSearchIngest:
   - The mcp_servers.jobs.headers entry is
       "Bearer ${run.credentials.jobs}"
   - Client.do() resolves to "Bearer <JobEmber-bearer-for-alice>"
   - Ingestion succeeds.

6. Same run also calls mcp__slack__send_message:
   - The mcp_servers.slack.headers entry is
       "Bearer ${run.credentials.slack}"
   - Client.do() resolves to "Bearer xoxp-<alice-slack-oauth>"
   - Slack DM posted to Alice.

7. Run completes; ctx torn down; credentials evicted from memory.
   The transcript record on the events table does NOT contain any
   of the three credential values.
```

The same workflow runs autonomously via RFC E's scheduled-agent-runs
substrate — schedule forks store the same map shape; sweeper builds
`RunInput` with it; flow continues identically from step 3 onwards.

## Reference: wire-field summary

| Transport | Field | Type |
|---|---|---|
| HTTP `POST /v1/runs` body | `user_credentials` | object<string, string> |
| HTTP `POST /v1/sessions/{id}/messages` body | `user_credentials` | object<string, string> |
| gRPC `RunRequest` | `user_credentials` (field 12) | map<string, string> |
| gRPC `ContinueRequest` | `user_credentials` (field 9) | map<string, string> |
| MCP `spawn_run` tool input | `user_credentials` | object<string, string> |
| TS adapter `RunOptions` | `userCredentials` | Record<string, string> |
| TS adapter `ContinueOptions` | `userCredentials` | Record<string, string> |

## Related topics

- `observability` — the per-span-attribute exclusion list. This
  RFC inherits that posture for credential values.
- `fairness` — per-user fairness applies on top of the credentials
  map; the `user_id` field is the quota anchor.
