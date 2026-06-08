---
name: input-webhooks
description: Inbound webhooks (WebhookDef) — let external systems (GitHub, Stripe, Linear, n8n) trigger agent runs or wake parked agents via signed HTTP POST. HMAC-over-raw-body auth, JSONPath payload mapping, spawn vs channel delivery, idempotency, rate limiting, per-run credentials, on_complete hooks, triage endpoints.
---

# Input webhooks (`WebhookDef`)

An input webhook turns an external HTTP POST into loomcycle work: an
external system (GitHub, Stripe, Linear, a CI server, n8n cloud) signs and
POSTs an event, and loomcycle either **spawns an agent run** (`delivery:
spawn`) or **publishes to a channel** to wake an agent already waiting on a
callback (`delivery: channel`). `WebhookDef` is the fifth substrate
primitive, alongside AgentDef / SkillDef / MCPServerDef / ScheduleDef.

## Why a substrate primitive, not glue code

Without this, every operator writes the same receiver: decode the GitHub
payload, verify `X-Hub-Signature-256` against a per-route secret, reshape
the body into a run request, POST it to `/v1/runs` with the operator's
bearer. That glue moves credential handling *out* of the substrate (where
the env-allowlist gate is the trust boundary) into bespoke code, and it is
re-implemented per source.

`WebhookDef` makes inbound triggers a first-class, versioned, signed-by-
default primitive — the same content-addressed CRUD as the other Defs, so
the operator mental model stays symmetric. Scheduled runs (ScheduleDef) and
webhook-triggered runs are the two halves of one problem — autonomous run
creation from a non-interactive source; the trigger shape (cron tick vs.
HTTP POST) is the only difference.

This is the **inbound** direction. For loomcycle calling *out* to your
service during a run, see the `hooks` topic (tool-use webhooks). For
loomcycle as an A2A peer, see `a2a-integration`.

## Enabling

Off by default. Set `LOOMCYCLE_WEBHOOKS_ENABLED=1`; the receiver mounts at
`POST /v1/_webhooks/{name}`.

**Secret resolution (read this first — it is the #1 setup snag).** A signing
secret / bearer token / `user_credentials_from_env` value resolves only if its
env-var NAME is authorized. The receiver authorizes a name when **any** of:

1. it is **`LOOMCYCLE_*`-prefixed** (or a known third-party name like
   `GITHUB_TOKEN`) — auto-allowed for the **verification secret** only
   (`signing_secret_env` / `bearer_token_env`), which is consumed by the
   receiver and never reaches the agent. This is why
   `signing_secret_env: "LOOMCYCLE_GITEA_WEBHOOK_SECRET"` Just Works with no
   allowlist config;
2. it is declared by a **static** (operator-authored yaml) webhook — its own
   secret + `user_credentials_from_env` names are auto-trusted;
3. it is listed in **`LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST`** (comma-separated) or
   the scheduler's shared **`LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST`**.

The agent-reachable `user_credentials_from_env` path does **not** get rule 1's
namespace auto-allow for *runtime*-authored (`webhookdef`-tool) defs — name a
non-static credential env explicitly in `LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST` so a
less-trusted authoring path can't inject an arbitrary env var into a run. A name
that resolves by none of the rules → `503 secret_unresolvable` (naming the env
var, never its value). The boot log prints the live allowlist count and a
`WARNING:` line per static webhook whose secret won't resolve.

**Trusted-network (no-auth) ingress.** For a receiver only reachable over an
already-authenticated transport (WireGuard/tailnet, mTLS mesh) where HMAC is
redundant, set `auth.kind: none`. It is refused (`503
unauthenticated_mode_disabled`) unless the operator opts in with
`LOOMCYCLE_WEBHOOKS_ALLOW_UNAUTHENTICATED=1` — the default never silently
accepts unsigned external POSTs.

## Defining a webhook

`WebhookDef` is a 5-op substrate tool (`create`/`fork`/`get`/`list`/
`retire`) with 4-transport admin (HTTP `POST /v1/_webhookdef`, gRPC, MCP
meta-tool, TS `webhookDef()`), or a static `webhooks:` yaml block.

```yaml
webhooks:
  # spawn mode (default): trigger a fresh agent run
  github-pr-review:
    enabled: true
    delivery: spawn
    agent: pr-review-agent
    auth:
      kind: hmac                      # hmac (default) | bearer
      header: "X-Hub-Signature-256"   # default X-Loomcycle-Signature (Stripe shape)
      signing_secret_env: "LOOMCYCLE_GITHUB_PR_REVIEW_SECRET"
      delivery_id_header: "X-GitHub-Delivery"
    rate_limit: { requests_per_minute: 60, burst: 10 }
    body_size_limit_bytes: 1048576
    user_credentials_from_env:        # operator-owned MCP bearers (env-allowlisted)
      github: "LOOMCYCLE_GITHUB_PR_REVIEW_TOKEN"
    payload_mapping:
      goal: "$.pull_request.title"
      user_id: "$.sender.login"
      user_credentials.github: "$.installation.access_token.value"  # per-event token; payload wins
    sync_response: { enabled: false, timeout_ms: 30000 }
    on_complete:
      - { kind: channel.publish, channel: "_system/pr-reviews", payload: { text: "reviewed" } }

  # channel mode: wake an agent parked on Channel.subscribe (no run, no creds)
  ci-build-done:
    enabled: true
    delivery: channel
    channel: "_system/webhook.ci-build-done"
    auth: { kind: hmac, signing_secret_env: "LOOMCYCLE_CI_WEBHOOK_SECRET", delivery_id_header: "X-Delivery-Id" }
    payload_mapping: { build_id: "$.build.id", status: "$.build.status" }
```

> **Known limitation (observed v0.23.x — experiments finding F30).** A `spawn`
> webhook's `agent:` must name a **yaml-declared** agent. An agent created at
> runtime via the **AgentDef substrate** (`POST /v1/_agentdef`, the `agentdef`
> MCP tool, `register_agent`) is **not resolved by the webhook-spawn path**: the
> delivery verifies its signature, then fails `rejected_spawn_setup` with
> `webhook "<name>": async run failed: unknown agent: <agent>`. Note the
> asymmetry — `POST /v1/runs` *does* resolve dynamically-created agents, so the
> gap is specific to the webhook receiver's agent lookup (it consults the static
> config, not the AgentDef store). Until fixed, point a `spawn` webhook at a
> yaml-declared agent (or wake a parked dynamic agent with `delivery: channel`,
> once F29's runtime-channel pub/sub gap is also resolved).

## How a request is processed

A shared front-half runs for every request, then forks on `delivery`:

1. **Resolve** the active `WebhookDef` by the `{name}` in the URL (never
   from the body). Unknown or disabled → opaque `404`.
2. **Read** the raw body under `body_size_limit_bytes` (default 1 MiB).
3. **Verify the signature over the raw bytes, before any parsing.**
   HMAC-SHA256 with a constant-time compare. Three envelopes auto-detected
   from the header *value* (so a custom header name still parses):
   - **Stripe** — `t=<unix>,v1=<hex>`, signs `<t>.<body>`, ±5-minute window
     (default header `X-Loomcycle-Signature`).
   - **GitHub** — `sha256=<hex>`, signs the body.
   - **bare hex** — the whole value is the hex MAC over the body, no prefix
     (Linear and many custom sources).
   Plus a **bearer** fallback (`kind: bearer`) for systems that can't sign.
4. **Replay/dedup** (Layer 1, in-memory, per `delivery_id`) + **idempotency**
   (Layer 2, durable `runs.idempotency_key`) so a re-delivered event lands
   on the same run instead of spawning twice.
5. **Project** the payload via the Def's `payload_mapping` (strict JSONPath
   subset: `$.a.b`, `$.a[0]` — no wildcards/filters/recursion). An absent
   path resolves to empty + a tracing note, never a failure.
6. **Rate limit** (per-Def token bucket) → `429` + `Retry-After` when
   exceeded.
7. **Deliver**: spawn → build a RunInput (the mapped `goal` enters as an
   **untrusted-block**, fenced in `<untrusted>` tags — a webhook payload is
   external, attacker-influenceable input) and run it; channel → publish +
   notify.

## The signing secret

`signing_secret_env` (HMAC) / `bearer_token_env` (bearer) name an env var the
receiver authorizes via the rules under **Enabling** above: a `LOOMCYCLE_*` (or
known third-party) name is auto-allowed; a static webhook's declared name is
auto-trusted; otherwise list it in `LOOMCYCLE_WEBHOOKS_ENV_ALLOWLIST` (or the
scheduler's shared `LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST`). The verification secret
is consumed by the receiver for the constant-time MAC/bearer compare and never
reaches the agent. A missing or unresolvable secret **fails loud**: `503
secret_unresolvable`, naming the env var — never its value. Rotate by changing
the env var and reloading; no plaintext secret ever lives in a Def.

## MCP-tool bearer tokens for the spawned run (same as schedulers)

A webhook-triggered **spawn** run is autonomous run creation from a
non-interactive source — exactly like a scheduled run — so it wires
MCP-tool bearers through the **same first-class field and the same resolver
as `ScheduleDef`**: `user_credentials_from_env`. There are two sources, and
both land on the run's `UserCredentials` map, which the MCP HTTP transport
substitutes into `${run.credentials.<name>}` in your `mcp_servers.*.headers`
(the RFC F seam) — so a webhook-spawned agent calls your authorized MCP
servers with per-user/service bearers, just as scheduled and interactive
runs do.

1. **`user_credentials_from_env`** — operator-owned, static, env-allowlist-
   gated. The value of each entry is an env-var NAME (validated
   `[A-Z][A-Z0-9_]*`), never a literal secret. This is the **identical
   field, identical semantics** as `ScheduleDef.user_credentials_from_env`.
   ```yaml
   user_credentials_from_env:
     github: "LOOMCYCLE_GITHUB_PR_REVIEW_TOKEN"   # → ${run.credentials.github}
     slack:  "LOOMCYCLE_SLACK_BOT_TOKEN"          # → ${run.credentials.slack}
   ```
2. **`payload_mapping` → `user_credentials.<name>`** — the webhook-specific
   addition: a *per-event* token projected from the inbound (verified) body,
   e.g. a GitHub App installation token. The payload value **overlays** (and
   wins over) the env-resolved one for the same key; an absent path does not
   clobber the env fallback.
   ```yaml
   payload_mapping:
     user_credentials.github: "$.installation.access_token.value"
   ```

Then reference them in the MCP server config exactly as any other run does:
```yaml
mcp_servers:
  github:
    transport: http
    url: "https://api.githubcopilot.com/mcp/"
    headers:
      Authorization: "Bearer ${run.credentials.github}"
```

**Channel-delivery** webhooks carry **no** credentials — a channel publish
has no run identity to attach per-user bearers to, and accepting them would
leak a secret onto the message bus. `user_credentials_from_env` (and any
`payload_mapping` `user_credentials.*` target) is **refused at create time**
for `delivery: channel` (RFC H Decision 11).

## `tenant_id` — which tenant the spawned run executes as (RFC N)

Set `tenant_id:` on a webhook def to make its **spawn** run execute as that
tenant: the run resolves that tenant's agents / skills / MCP servers and its
memory and run records are isolated to the tenant (RFC L multi-tenant
boundary). Omit it (`""`) for a shared/default run with no tenant scoping.

```yaml
  github-pr-review:
    delivery: spawn
    agent: reviewer
    tenant_id: acme            # spawned run executes as tenant "acme"
```

**Security — the tenant comes from the static def ONLY, never the payload.**
Unlike `user_id` / `user_tier` (which an operator MAY project from the signed
body via `payload_mapping`, accepting that they are only as trustworthy as
the per-def signing secret), there is deliberately **no** `payload_mapping`
or `run_metadata.*` path for the tenant. The inbound body is attacker-
influenceable; letting it select the tenant would let a sender steer the run
into another tenant's agents/skills/memory. `tenant_id` is def-content
(operator-authored), flows to `RunInput.TenantID`, and cannot be overridden
from the wire.

## Response policy

`202 Accepted` with `{run_id, webhook_name, delivery_id}` (async, the
default). `?sync=true` (when the Def's `sync_response.enabled`) blocks on the
run-state bus until the run reaches a terminal state — `200` with the
status, or `504` on `sync_timeout_ms`. Channel mode returns `202`.

## Never silently degrade

Every outcome is loud and distinct: `404` unknown/disabled (opaque, no
enumeration), `401` signature/auth failure (no body detail — no oracle),
`503 secret_unresolvable`, `400` malformed body/mapping, `429` rate limit,
`503` runtime unavailable. A rate-limited or rejected delivery does **not**
burn its `delivery_id`, so the sender's retry is processed rather than
dropped as a replay.

A **replayed valid delivery** (a re-send of an already-accepted
`delivery_id`, within the dedup window) is **not** an error — it is only
reachable after the signature has already verified, so it is an idempotent
re-delivery, not an attacker. The receiver acks it **`200`** with
`{deduped: true}` and the original `run_id` (when the spawn path recorded
one), matching the GitHub/Stripe redelivery contract, and does **not** spawn
a second run. (A sender that retries on non-2xx therefore stops, rather than
rotating its secret on a misread `401`.)

## on_complete

Spawn-delivery webhooks may declare `on_complete` hooks that fire after the
run finishes: `channel.publish` and `memory.set` are wired; `mcp.call` is
reserved (logged + skipped until an MCP caller is wired, same as the
scheduler). A hook failure is logged and never affects the run.

## Triage (admin-gated)

Two bearer-authed endpoints help debug a webhook that's silently failing
(the receiver POST itself is unauthed — it uses the per-Def secret):

- `GET /v1/_webhooks/{name}/recent-deliveries?limit=50` — the last N
  invocations with `delivery_id`, `verdict`
  (`accepted`/`accepted_replay`/`rejected_sig`/`rejected_rate`/
  `unresolvable_secret`/…), `received_at`, `run_id`.
- `POST /v1/_webhooks/{name}/test` — dry-run: POST a sample body + signature
  and get back `{would_accept, verdict, run_input_preview}` (credential
  **key names** only, never values). No run is created.

## Provider recipes

Copy-paste starting points for well-known sources. Each shows the
`WebhookDef` (the `op: create` body is the same object under `"op":"create",
"name":"…"`), the env vars to set, and where the provider's secret comes
from. All are verified against the receiver's signature handling.

### GitHub — PR opened → review it (spawn)

GitHub signs the raw body HMAC-SHA256 as `X-Hub-Signature-256: sha256=…`,
with a unique `X-GitHub-Delivery` id. Carry the org's GitHub MCP bearer from
the operator env, and (optionally) overlay the per-event App installation
token from the payload.

```yaml
webhooks:
  github-pr-review:
    enabled: true
    delivery: spawn
    agent: pr-review-agent
    auth:
      kind: hmac
      header: "X-Hub-Signature-256"
      signing_secret_env: "LOOMCYCLE_GITHUB_WEBHOOK_SECRET"
      delivery_id_header: "X-GitHub-Delivery"
    user_credentials_from_env:
      github: "LOOMCYCLE_GITHUB_TOKEN"            # → ${run.credentials.github}
    payload_mapping:
      goal:    "$.pull_request.title"
      user_id: "$.sender.login"
      user_credentials.github: "$.installation.access_token.value"  # per-event token wins
```
GitHub repo settings → Webhooks → Secret = `LOOMCYCLE_GITHUB_WEBHOOK_SECRET`;
content type `application/json`.

### Stripe — payment event (spawn or channel)

Stripe's `Stripe-Signature: t=…,v1=…` signs `<t>.<body>` (the receiver's
default envelope). Stripe sends no delivery-id header, so omit
`delivery_id_header` — the body-hash fallback dedups identical events (each
event's unique `id` is in the body).

```yaml
webhooks:
  stripe-payment:
    enabled: true
    delivery: spawn
    agent: billing-agent
    auth:
      kind: hmac
      header: "Stripe-Signature"
      signing_secret_env: "LOOMCYCLE_STRIPE_WEBHOOK_SECRET"   # whsec_…
    payload_mapping:
      goal: "$.type"                  # e.g. "invoice.payment_failed"
      run_metadata.event_id: "$.id"
```

### Linear — issue created (spawn)

Linear sends a bare hex HMAC-SHA256 of the body in `Linear-Signature` (no
prefix, no timestamp — the bare-hex envelope).

```yaml
webhooks:
  linear-issue:
    enabled: true
    delivery: spawn
    agent: triage-agent
    auth:
      kind: hmac
      header: "Linear-Signature"
      signing_secret_env: "LOOMCYCLE_LINEAR_WEBHOOK_SECRET"
    payload_mapping:
      goal:    "$.data.title"
      user_id: "$.data.creator.email"
```

### GitLab — merge request (spawn, shared-secret token)

GitLab does not sign; it sends a raw shared secret in `X-Gitlab-Token`. Use
`kind: bearer` with `header:` set so the receiver compares that header's raw
value constant-time.

```yaml
webhooks:
  gitlab-mr:
    enabled: true
    delivery: spawn
    agent: mr-review-agent
    auth:
      kind: bearer
      header: "X-Gitlab-Token"
      bearer_token_env: "LOOMCYCLE_GITLAB_WEBHOOK_TOKEN"
    payload_mapping:
      goal:    "$.object_attributes.title"
      user_id: "$.user.username"
```

### Generic / internal service or n8n (bearer, Authorization)

Any source that can send `Authorization: Bearer <token>` (n8n, Zapier, an
internal service). Omit `header:` for the standard Authorization shape.

```yaml
webhooks:
  internal-event:
    enabled: true
    delivery: spawn
    agent: ops-agent
    auth:
      kind: bearer
      bearer_token_env: "LOOMCYCLE_INTERNAL_WEBHOOK_TOKEN"
    payload_mapping:
      goal: "$.message"
```

### CI build done → wake a waiting agent (channel)

An agent parked on `Channel.subscribe("_system/webhook.ci-build-done")` is
woken when the build callback arrives. No run, no credentials.

```yaml
webhooks:
  ci-build-done:
    enabled: true
    delivery: channel
    channel: "_system/webhook.ci-build-done"
    auth:
      kind: hmac
      signing_secret_env: "LOOMCYCLE_CI_WEBHOOK_SECRET"   # default X-Loomcycle-Signature envelope
      delivery_id_header: "X-Delivery-Id"
    payload_mapping:
      build_id: "$.build.id"
      status:   "$.build.status"
```

### Signature schemes — supported vs not yet

Supported today: HMAC-SHA256 over the raw body as **`sha256=<hex>`**
(GitHub), **`t=,v1=<hex>`** over `<t>.<body>` (Stripe), **bare `<hex>`**
(Linear and custom sources), and a **shared-secret bearer** (in
`Authorization` or a custom header — GitLab/n8n/Zapier/internal).

Not yet supported (the secret resolves but verification would fail —
prefer the bearer fallback or an upstream verifier for these until added):
**base64-encoded** HMAC digests (e.g. Shopify's `X-Shopify-Hmac-Sha256`),
and custom signed-payload constructions that sign more than the raw body or
`<t>.<body>` (e.g. Slack's `v0:<ts>:<body>` with the timestamp in a separate
header).

## Caveats

- **Single-replica v1.** The Layer-1 dedup cache + rate-limit buckets are
  per-replica; the durable `runs.idempotency_key` (Layer 2) is the
  cross-replica backstop for spawn mode. Cluster-wide dedup/rate-limit is a
  later concern.
- **Not a DDoS shield.** Signature verification is cheap and the body is
  size-capped, but front-line flood protection belongs at your ingress; the
  per-Def rate limit caps *accepted* invocations (runtime protection), not
  unauthenticated noise.

**Bottom line:** `WebhookDef` is the signed-by-default front door for
external events — one substrate shape for "an outside system wants to start
(or wake) an agent," with the verify-before-parse, fail-loud, retry-safe
discipline a trust boundary needs.
