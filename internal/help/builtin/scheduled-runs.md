---
name: scheduled-runs
description: Scheduled autonomous agent runs — operator-authored cron schedules + dynamic per-user forks + on_complete delivery hooks. v1.x substrate.
---
Loomcycle v1.x introduces a ScheduleDef substrate primitive
(parallel to AgentDef / SkillDef / MCPServerDef) that lets operators
express "build this `RunInput` on this cron, for this user, with
these credentials." The scheduler is reframed as an entity that
creates standard agentic runs — per-tool authorization (the
`user_credentials` map) flows through unchanged. The scheduler
is transparent: once a scheduled run is in flight, it's
indistinguishable from a `POST /v1/runs` caller's run that
supplied the same credentials map directly.

This is **off by default** in the sense that operators with no
`scheduled_runs:` yaml entries see no sweeper activity. The
substrate tables exist; the substrate-CRUD path (future) accepts
dynamic forks but nothing fires until something is scheduled.

## Yaml shape (two entry styles)

```yaml
scheduled_runs:
  # ─── Template style — orchestrators fork per user ───
  job-search-template:
    agent: job-search-batch
    prompt:
      - role: user
        content:
          - type: trusted-text
            text: "Run the daily job search batch. EXPECTED_RESULTS: 30"
    user_tier_schedules:
      low:    "0 6 1,11,21 * *"        # 3×/month
      middle: "0 6 1,8,15,22 * *"      # 4×/month
      high:   "0 6 * * *"              # daily
    required_credentials: [jobs, slack, telegram]
    timezone: "Europe/Berlin"
    enabled: true
    on_complete:
      - kind: mcp.call
        server: telegram
        tool: send_message
        args:
          chat_id: "{{user.telegram_chat_id}}"
          text: "{{run.final_text}}"

  # ─── Standalone style — operator-owned cron ───
  alarm-summary-weekly:
    agent: alarm-summarizer
    user_id: operator@example.com
    user_credentials_from_env:
      slack: LOOMCYCLE_OPERATOR_SLACK_BEARER
      jobs:  LOOMCYCLE_OPERATOR_JOBS_BEARER
    schedule: "0 9 * * 1"              # Monday 09:00
    prompt:
      - role: user
        content:
          - type: trusted-text
            text: "Summarise the past week's alarms."
    timezone: "UTC"
    enabled: true
    on_complete:
      - kind: channel.publish
        channel: _system/operator-digest
        payload: { text: "{{run.final_text}}" }
```

Two entry styles share the same struct shape; the validator detects
which by `user_id != ""`. A template has no `user_id` and offers
`user_tier_schedules` per-tier cron defaults plus a
`required_credentials` manifest that forks must satisfy. A
standalone entry has an explicit `user_id` and a single `schedule:`.

Mutual exclusion: a template can't fix one cron AND offer per-tier
defaults. The config validator refuses at boot.

## `max_fires` — bounded / one-shot schedules

`max_fires: N` caps the schedule's LIFETIME fire count. The sweeper
auto-retires the def after its Nth fire — no external watcher needed.

```yaml
scheduled_runs:
  one-shot-migration:
    agent: migrator
    schedule: "*/5 * * * *"   # next 5-min boundary
    max_fires: 1              # fire exactly once, then retire
    prompt:
      - role: user
        content: [{ type: trusted-text, text: "Run the migration." }]
```

- `0` (default) = fire indefinitely until retired — unchanged behavior.
- `1` = one-shot (pair with a near-future cron for "run once soon").
- `N > 1` = a finite run of N fires.

Fires of **any** status count (completed / failed / backpressure-skipped)
so a wedged schedule still retires; catch-up fires after a pause count
too — it's a hard lifetime cap regardless of cadence. The disabled-skip
advance (`enabled: false`) does NOT count, so toggling a schedule off and
on preserves its remaining budget. Retirement flips the def's `retired`
flag (lineage stays visible in `/ui/schedules`); the run-state row's
`fire_count` is the counter. Set it on a substrate fork via the overlay:
`{op:"fork", name:"…", overlay:{max_fires:3}}`.

## `tenant_id` — which tenant the fired run executes as

Set `tenant_id:` on a schedule to make its spawned run execute as that
tenant: the run resolves that tenant's agents / skills / MCP servers and
its memory and run records are isolated to the tenant (multi-tenant
boundary). Omit it (`""`) for a shared/default run with no tenant scoping.

```yaml
  nightly-acme-digest:
    agent: digest
    tenant_id: acme            # run executes as tenant "acme"
    schedule: "0 2 * * *"
    user_id: ops@acme.example
```

It is def-content (lives in the definition, participates in the def's
serialized identity), so a schedule that runs as tenant A is a genuinely
different def than one running as tenant B. The scheduler has no inbound
payload, so there is no way for an external value to set the tenant. It
flows to `RunInput.TenantID`.

**Through the ScheduleDef tool, `tenant_id` may name only your own tenant
unless you are an admin.** Omitted, it defaults to your tenant. A
non-admin `create` or `fork` whose `tenant_id` names another tenant is
refused, and so is a `fork` or hook edit of a definition whose runs
already execute in another tenant — set `tenant_id` to your own tenant in
the fork's overlay to re-home it. The operator's yaml may still name any
tenant, and so may the operator on an unauthenticated deployment or through
the local stdio MCP server, which has no token to carry the admin scope.

A schedule with no tenant runs in the shared operator layer, but writing one
there does not make it the operator's. Only a schedule an admin wrote with no
tenant — or the yaml's own — may run a memory consolidation fan-out across
every tenant; see `memory-consolidation`.

A stored schedule whose definition names no tenant but that a tenant owns —
written before definitions recorded the tenant — runs, publishes and fires its
hooks in the tenant that owns it. Re-save it to record the tenant. Webhooks
follow the same rule.

## Schedules restored from a snapshot

A snapshot carries every schedule with its run state, so a restored
schedule comes back **active** and keeps its `fire_count`: a `max_fires: 5`
schedule that fired 3 times has 2 fires left. A `next_run_at` already in
the past fires once, on the first sweep after the runtime resumes.

Literal `user_credentials` values never travel in a snapshot (references
such as `$cred:<name>` and `user_credentials_from_env` names do). A
schedule that lost a literal comes back with `enabled: false` and a
server-set marker:

```json
"capture_disabled": {"stripped_credentials": ["jobs", "slack"]}
```

To re-enable it, `fork` it with every listed key in `user_credentials` or
`user_credentials_from_env` and `enabled: true`. A key counts only when the
scheduler will use it: a `user_credentials` value that is not blank, or a
`user_credentials_from_env` variable that is on
`LOOMCYCLE_SCHEDULER_ENV_ALLOWLIST` (empty by default) and set. A fork that
supplies only some of the keys stays disabled and keeps the marker with the
keys still missing; its result lists them in
`disabled_until_credentials_supplied`, and `unusable_credentials` says why a
key you did name did not count. Either way the fork starts from the fire
count already spent, never from zero. A `create` on the same name writes a new version of it
and follows the same rules: it keeps the marker unless it supplies every
listed key, and it keeps the fire count. The marker cannot be set or
cleared through an overlay.

A snapshot is a copy, not a lease: restoring one into a second instance
while the first keeps running fires every enabled schedule on both, and
together they can exceed `max_fires`. Disable the schedules on one side.

## What ships in the v1.x.0 substrate PR

This is the data-layer foundation. The agent-facing tool +
scheduler runtime + 4-transport CRUD + Web UI tab ship in
follow-up PRs.

- ✅ `schedule_defs` + `schedule_def_active` + `schedule_run_state`
  tables (Postgres + SQLite migrations)
- ✅ Store interface: `ScheduleDefCreate` / `Get` /
  `GetByNameVersion` / `ListByName` / `ListChildren` /
  `ListNames` / `SetActive` / `GetActive` / `SetRetired`
- ✅ Storetest contract tests (7 cases)
- ✅ `cfg.ScheduledRuns` yaml block + `ScheduledRun` struct
- ✅ Config-load validation (cron syntax, agent name resolution,
  on_complete closed-set kinds, schedule-vs-tier-schedules mutual
  exclusion)
- ✅ `lookup.Schedule(ctx, store, cfg, name)` canonical resolver
  walking static → substrate
- ✅ `SubstrateScheduleDef` JSON-tagged adapter + drift test
- ⏳ `ScheduleDef` built-in tool (5 ops: create, fork, get, list,
  retire) — **next PR**
- ⏳ `internal/scheduler/` package — sweeper goroutine + cron
  parsing + on_complete dispatch — **next PR**
- ⏳ 4-transport CRUD (HTTP + gRPC + MCP `scheduledef` tool + TS
  adapter) — **next PR**
- ⏳ `/ui/schedules` Web UI tab — **follow-up PR**

## Per-tool credentials

This RFC stores the `user_credentials` map in fork rows and passes
it through `RunInput.UserCredentials` when the sweeper fires. All
authorization semantics — substitution syntax
`${run.credentials.<name>}`, sub-agent identity inheritance, the
transcript-exclusion posture, the v0.8.14 back-compat sugar —
live in per-run credentials (see `Context.help per-run-credentials`).

The only credential-related concept introduced by this RFC is the
template-side `required_credentials:` list that forks must
populate; the `fork` op (future) will refuse with
`ErrCredentialsIncomplete` if any required key is missing.

## Closed-set delivery hooks

`on_complete` accepts three hook kinds and no others. The closed
set prevents the hook config from becoming a parallel scripting
surface; agents wanting custom delivery use tool calls inside the
run, not the hook surface.

| Kind | Required fields |
|---|---|
| `channel.publish` | `channel` + `payload` |
| `mcp.call` | `server` + `tool` + `args` |
| `memory.set` | `scope` + `key` + `payload` (as the memory value) |

Mcp.call hook dispatch reuses the per-tool credentials map —
the substituted credential value is whatever the schedule fork's
`user_credentials[server]` resolves to.

A hook writes into the tenant its run executed in. For an ordinary
schedule that is the schedule's own tenant (`tenant_id`, or the operator
layer when it has none). An operator's consolidation schedule runs each
pass in its user's own tenant, so its hooks fire once per tenant whose
passes all completed, in that tenant, naming that tenant's run — as one
schedule per tenant would. See `memory-consolidation`.

A `delivery: channel` tick starts no run, so it lands in the schedule's
own tenant. On a `scope: global` channel a schedule with no `tenant_id`
publishes into the operator layer, which every tenant reads; give the
schedule a `tenant_id` to reach one tenant's channel instead.

## Cron syntax

Standard 5-field cron expressions per
`github.com/robfig/cron/v3`'s `ParseStandard`:

```
┌───────────── minute (0–59)
│ ┌─────────── hour (0–23)
│ │ ┌───────── day of month (1–31)
│ │ │ ┌─────── month (1–12)
│ │ │ │ ┌───── day of week (0–6; 0 = Sunday)
│ │ │ │ │
* * * * *
```

A sharp edge: no natural-language phrases like "every
weekday at noon." If you need scheduling beyond cron's expressive
range, set up multiple entries.

## Related topics

- `per-run-credentials` — the per-tool credentials map. The
  scheduler stores credentials in fork rows + passes them through
  RunInput; that topic documents the wire shape + substitution semantics +
  sub-agent inheritance + v0.8.14 back-compat sugar.
- `fairness` — per-user fairness applies to scheduled runs identically
  to on-demand runs (same `Runner.RunOnce` path; same `user_id`
  quota anchor).
- `observability` — scheduled runs emit the same OTEL spans
  on-demand runs do. The sweeper itself emits no spans; only the
  spawned `loomcycle.run` spans (one per fire) appear in traces.
