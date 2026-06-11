---
name: pause-resume-snapshot
description: Runtime-wide quiesce + cross-version-portable JSON snapshot (v0.8.17). Pause / Resume / Snapshot / Restore / Export against a running instance.
---
v0.8.17 ships the runtime-wide quiesce + restore primitive.
Operators drive it; agents never call these directly.

## What the three operations do

**Pause** declares the runtime is winding down. The in-flight
state machine transitions `running → pausing → paused`:

- Idempotent tools (Read, WebFetch, WebSearch, Memory.get/list,
  Channel.peek, AgentDef.get/list, Context.*, Evaluation.get/
  aggregate) are cancelled IMMEDIATELY — their ctx flips to Done
  the moment pause is declared. The dispatcher returns IsError=true
  and the agent loop records `pause_state='paused'` at the next
  iteration boundary.
- Non-idempotent tools (Bash, Write, Edit, HTTP-mutate, Memory.set/
  incr/delete, Channel.publish/subscribe/ack, AgentDef.create/
  fork/promote/retire, Evaluation.submit) and external (MCP)
  tools are given a grace window — 30 s by default — to finish
  cleanly. Any still running at the deadline get force-cancelled
  and counted in the result.
- New `/v1/runs` requests (and the gRPC / webhook / A2A run-admission
  paths) return 503 / Unavailable while the runtime is in `pausing` or
  `paused`; the scheduler skips firing. Sub-agents of an already-admitted
  run are NOT rejected — they park at their own boundary.
- Pause WAITS (up to `timeout_ms`) for in-flight runs to reach a boundary
  and park, so `paused_runs_count` is meaningful on return. A run blocked
  inside a single long tool / provider turn parks at its NEXT boundary; if
  it doesn't reach one within the window, Pause returns with a warning
  naming it (it parks on its next boundary regardless). Snapshot only
  captures `pause_state='paused'` runs, so wait for a clean Pause result
  (no "did not reach a boundary" warning) before snapshotting mid-run.

**Resume** flips back to `running`. Each previously-paused run's
state row is updated; the runner goroutine watching the broadcast
channel re-enters the loop. Resume is intentionally CHEAP — it
just clears the brakes; the runs drive themselves forward.

**Snapshot** captures running-state into a JSON envelope:
seven sections (`agent_defs`, `agent_def_active`, `memory`,
`channels`, `evaluations`, `paused_runs`, optional `interaction_
history`). Each section carries its own version. The envelope is
stored in the new `snapshots` table; export streams the raw JSON
to the operator.

## When to use each

| Operator task | What to run |
|---|---|
| Provider rate-limited, want to drain in-flight work | `loomcycle pause` |
| Wait for capacity / quota / maintenance window | leave it paused |
| Resume from where each run left off | `loomcycle resume` |
| Pre-backup quiesce | `loomcycle pause`, run your backup tool, `loomcycle resume` |
| Capture state for archival or debugging | `loomcycle snapshot --description "..."` |
| Migrate to a new VM or new loomcycle version | `loomcycle snapshot` → `loomcycle snapshots export <id> --out file.json` → on the destination: `loomcycle restore file.json` |
| Inspect current state | `loomcycle state` (or `GET /v1/_state`) |

Pause and snapshot are independent operations. Compose them as
needed: pause alone for rate-limit-wait, pause + snapshot + export
for migration, snapshot during running for backup-time-of-day
capture (read-only, no agent disruption).

## Wire surface

All endpoints are bearer-authed; same posture as `/v1/_users` and
`/v1/_metrics`. No agent-facing surface.

```
POST /v1/_pause           body: {"timeout_ms": 30000?}
POST /v1/_resume
GET  /v1/_state

POST   /v1/_snapshots                     body: {"label?": "..."}
GET    /v1/_snapshots[?limit=&label_contains=]
GET    /v1/_snapshots/{id}                — full JSON envelope
DELETE /v1/_snapshots/{id}
GET    /v1/_snapshots/{id}/export         — raw JSON with Content-Disposition
POST   /v1/_snapshots/{id}/restore        body: {"include_history?": false, "json?": <envelope>}
```

The CLI subcommands (`loomcycle pause / resume / state / snapshot
/ snapshots list/export/delete / restore`) are thin HTTP clients
that default to `LOOMCYCLE_BASE_URL` + `LOOMCYCLE_AUTH_TOKEN` env.

Web UI surface: PauseControls in the topbar shows the current
state pill + Pause/Resume button; `/ui/snapshots` is the admin
page for capture / restore-from-file / export-as-download / delete.

The **same surface** is exposed through every wire transport
(v0.8.18+) — orchestrators pick whichever protocol fits:

- **HTTP** (above): canonical wire shape; the CLI + Web UI talk it.
- **gRPC** (`proto/loomcycle.proto`): 9 RPCs covering the full
  surface (`PauseRuntime`, `ResumeRuntime`, `GetRuntimeState`,
  `CreateSnapshot`, `ListSnapshots`, `GetSnapshot`, `ExportSnapshot`,
  `RestoreSnapshot`, `DeleteSnapshot`). Typed errors map to
  `Unavailable` / `FailedPrecondition` / `NotFound` /
  `ResourceExhausted` status codes.
- **LoomCycle MCP** (`loomcycle mcp`): 9 of the 22 meta-tools
  cover this surface (`pause_runtime`, `resume_runtime`,
  `get_runtime_state`, plus the 6 snapshot ops). External
  orchestrators (Claude Code etc.) drive it through standard MCP.
- **Python adapter** (`pip install loomcycle`, v0.6.0+): 9 async
  methods on `LoomcycleClient` with typed exception subclasses
  (`AlreadyPausingError`, `SnapshotNotFoundError`, etc.).

## Restore semantics

Restore is idempotent: each section's per-row insert is
`ON CONFLICT DO NOTHING` (Postgres) / `INSERT OR IGNORE` (SQLite).
A re-restore reports `0` in the counter for every section whose
rows already exist — the counter is `rows_actually_written`, not
`rows_attempted`.

`paused_runs` reference session_ids, but sessions aren't a
captured section. Restore synthesizes a session row deterministically
(`snap_sess_<run_id>`) before inserting the run; the
`synthesized_sessions` counter on the response surfaces this so
operators can audit cross-instance restore behaviour.

Per-section semver gates compatibility. A snapshot at section
version `1.0` restored on a reader at `1.0` is identity-decoded.
A reader at a newer section version walks a registered migration
chain (none today; all sections at `1.0`). A snapshot at a section
version newer than the reader is refused with
`ErrSnapshotVersionTooNew` — operators upgrade loomcycle before
restoring.

## Cross-instance / cross-version migration

The snapshot envelope is portable: export from one loomcycle
instance, restore on another. The Memory section's schema reserves
an optional `embedding` field (always null in v0.8.17; populated
by v0.9.x semantic memory) so a snapshot captured today round-
trips cleanly through a future loomcycle that does have vector
ops — no v1.0 → v1.1 schema migration of the just-shipped data.
This is the additive-fields forward-compat rule, locked in the
v0.8.17 RFC.

## What this is NOT

- **Not a backup.** Snapshot scope is running-state only (paused
  runs, Memory, Channel, agent_defs, evaluations). External DB
  backups handle archival history. The optional `include_history`
  flag adds an interaction-history section but is not how you
  back up a busy production database.
- **Not per-tenant.** Pause is runtime-wide. Per-tenant fairness
  defers to v0.9.x.
- **Not a transactional snapshot.** Sections are read in order;
  a row inserted between section reads will appear in one but
  not the other. For strict point-in-time consistency, pause
  first, then snapshot, then resume.
- **Not encrypted at rest.** Operator's disk-encryption policy
  applies — same posture as transcripts and Memory.

## Pair with Context.history

Restored experiments contain interaction history events
(opt-in via `include_history=true`). Agents in the restored
instance read those events via `Context.history(since_ts=
<experiment_start_ts>)` to reflect on past conversations.
Self-evolving experiments survive cross-version migration with
their memory intact.

## State-change signals

State transitions (`running → pausing → paused → running`) publish
to the operator-declared `_system/runtime-state` channel (v0.8.6
system channels). Operator dashboards and external orchestrators
subscribe to that channel via the existing Channel interface —
no new SSE event types were added for pause / resume.
