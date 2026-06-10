---
name: scheduled-runs
description: Scheduled autonomous agent runs (RFC E) — operator-authored cron schedules + dynamic per-user forks + on_complete delivery hooks. v1.x substrate.
---
Loomcycle v1.x introduces a ScheduleDef substrate primitive
(parallel to AgentDef / SkillDef / MCPServerDef) that lets operators
express "build this `RunInput` on this cron, for this user, with
these credentials." The scheduler is reframed as an entity that
creates standard agentic runs — per-tool authorization (RFC F's
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

## `max_fires` — bounded / one-shot schedules (RFC S / F36)

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

## `tenant_id` — which tenant the fired run executes as (RFC N)

Set `tenant_id:` on a schedule to make its spawned run execute as that
tenant: the run resolves that tenant's agents / skills / MCP servers and
its memory and run records are isolated to the tenant (RFC L multi-tenant
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
different def than one running as tenant B. It is **operator-authored
only** — the scheduler has no inbound payload, so there is no way for an
external value to set the tenant. It flows to `RunInput.TenantID`.

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

## Per-tool credentials are RFC F's concern

This RFC stores the `user_credentials` map in fork rows and passes
it through `RunInput.UserCredentials` when the sweeper fires. All
authorization semantics — substitution syntax
`${run.credentials.<name>}`, sub-agent identity inheritance, the
transcript-exclusion posture, the v0.8.14 back-compat sugar —
live in RFC F (see `Context.help per-run-credentials`).

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

Mcp.call hook dispatch reuses RFC F's per-tool credentials map —
the substituted credential value is whatever the schedule fork's
`user_credentials[server]` resolves to.

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

Per RFC E sharp edge: no natural-language phrases like "every
weekday at noon." If you need scheduling beyond cron's expressive
range, set up multiple entries.

## Related topics

- `per-run-credentials` — RFC F per-tool credentials map. The
  scheduler stores credentials in fork rows + passes them through
  RunInput; F documents the wire shape + substitution semantics +
  sub-agent inheritance + v0.8.14 back-compat sugar.
- `fairness` — per-user fairness applies to scheduled runs identically
  to on-demand runs (same `Runner.RunOnce` path; same `user_id`
  quota anchor).
- `observability` — scheduled runs emit the same OTEL spans
  on-demand runs do. The sweeper itself emits no spans; only the
  spawned `loomcycle.run` spans (one per fire) appear in traces.
