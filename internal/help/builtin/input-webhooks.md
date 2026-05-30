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
`POST /v1/_webhooks/{name}`. Signing secrets + per-run credentials resolve
through the **same env-allowlist** the scheduler uses
(`LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST`) — a value not on the allowlist is
never read.

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

## How a request is processed

A shared front-half runs for every request, then forks on `delivery`:

1. **Resolve** the active `WebhookDef` by the `{name}` in the URL (never
   from the body). Unknown or disabled → opaque `404`.
2. **Read** the raw body under `body_size_limit_bytes` (default 1 MiB).
3. **Verify the signature over the raw bytes, before any parsing.**
   HMAC-SHA256 with a constant-time compare. Two envelopes auto-detected:
   Stripe (`X-Loomcycle-Signature: t=<unix>,v1=<hex>`, signs `<t>.<body>`,
   ±5-minute window) and GitHub (`X-Hub-Signature-256: sha256=<hex>`, signs
   the body). Bearer fallback for systems that can't sign.
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

## Auth, secrets, credentials

- `signing_secret_env` / `bearer_token_env` name an env var that must be on
  the allowlist; a missing/unresolvable secret **fails loud** (`503
  secret_unresolvable`, naming the env var — never its value).
- **spawn** credentials: `user_credentials_from_env` (env-allowlisted,
  operator-owned) merged with `payload_mapping` targets under
  `user_credentials.<name>` (per-event tokens; the payload value wins). They
  land on the run's `${run.credentials.<name>}` substitution seam (RFC F).
- **channel** mode carries **no** credentials (a publish has no run identity)
  — declaring them on a channel Def is refused at create time.

## Response policy

`202 Accepted` with `{run_id, webhook_name, delivery_id}` (async, the
default). `?sync=true` (when the Def's `sync_response.enabled`) blocks on the
run-state bus until the run reaches a terminal state — `200` with the
status, or `504` on `sync_timeout_ms`. Channel mode returns `202`.

## Never silently degrade

Every failure mode is loud and distinct: `404` unknown/disabled (opaque, no
enumeration), `401` any auth/replay failure (no body detail — no oracle),
`503 secret_unresolvable`, `400` malformed body/mapping, `429` rate limit,
`503` runtime unavailable. A rate-limited or rejected delivery does **not**
burn its `delivery_id`, so the sender's retry is processed rather than
dropped as a replay.

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
  (`accepted`/`rejected_sig`/`rejected_replay`/`rejected_rate`/
  `unresolvable_secret`/…), `received_at`, `run_id`.
- `POST /v1/_webhooks/{name}/test` — dry-run: POST a sample body + signature
  and get back `{would_accept, verdict, run_input_preview}` (credential
  **key names** only, never values). No run is created.

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
