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

**Snapshot** captures running-state into a JSON envelope. Its
sections, grouped:

- **Identities and budgets:** `users`, and `token_limits` with this
  month's usage.
- **Definitions,** each kind with its active pointers where it has
  them: agents, skills, teams, hooks, MCP servers, channels, webhooks,
  schedules, A2A agents and server cards, memory backends, document
  sources, and dynamic volumes.
- **Memory:** `memory`, `memory_pending` (items queued but not yet
  consolidated), and `sqlmem` (SQL Memory, when it is enabled).
- `channels`, `evaluations`, `paused_runs`, and optionally
  `interaction_history`.
- `dirents`, the Path tree.

Each section carries its own version. The envelope is stored in the
`snapshots` table; export streams the raw JSON to the operator.

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
- **LoomCycle MCP** (`loomcycle mcp`): 9 meta-tools
  cover this surface (`pause_runtime`, `resume_runtime`,
  `get_runtime_state`, plus the 6 snapshot ops). External
  orchestrators (Claude Code etc.) drive it through standard MCP.
- **Python adapter** (`pip install loomcycle`, v0.6.0+): 9 async
  methods on `LoomcycleClient` with typed exception subclasses
  (`AlreadyPausingError`, `SnapshotNotFoundError`, etc.).

## Restore semantics

Restore is idempotent: a row whose key is already on the target is left
alone, so a re-restore reports `0` in the counter for every section whose
rows already exist — the counter is `rows_actually_written`, not
`rows_attempted`. For a definition the key is its `def_id`, on both
backends: a **different** definition on a name and version the target
already has (its own yaml bootstrap, say) is not written, and the restore
warns, naming it. The live definition stands.

### What restore reports

Every transport returns a `restored` map with every counter by name
(`agent_defs`, `memory`, `dirents`, …), each counting rows actually
written. A few count what the restore held back instead:

- each `<section>_refused` and `active_pointers_refused` count rows this
  host refused, which were not written;
- `defs_disabled_for_credentials` counts schedules and webhooks written
  **disabled** because the snapshot stripped their literal credentials.

The CLI prints those on a separate `not restored` line, and the Web UI
shows them as a warning. `paused_runs_resumed` counts the restored paused
runs relaunched on this instance, whichever transport ran the restore; a
run that cannot resume is marked failed and named in the warnings.

### Users and token limits

`users` and `token_limits` restore first, before any definition or
resumed run. A user or a budget the target already has stands: the live
row is kept, and a warning names a user the snapshot has stricter
(isolated or disabled) than the live one.

Month-to-date usage travels with the budgets, so a tenant near its limit
on the source is near it on the target. The carried total is kept as the
larger of the carry already stored and the snapshot's, so restoring the
same snapshot twice counts it once. Usage from an earlier month is not
carried: the budget window has rolled over.

### Definitions: agents, skills, teams, hooks, MCP servers, channels

**Every restored definition is re-validated** with the rules an author
faces on this host, and one that fails is not restored. The warning names
it and the reason, a pointer at it is not restored either, and the
section's `<section>_refused` counter (`mcp_server_defs_refused`,
`hook_defs_refused`, …) counts it. The rest of the section lands. What
each is checked for:

- **MCP servers:** the transport, and for http a url whose host is on
  **this host's** `LOOMCYCLE_HTTP_HOST_ALLOWLIST` or
  `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`; a stdio server only when this
  host sets `LOOMCYCLE_MCP_ALLOW_DYNAMIC_STDIO`. A server that is not
  restored is never dialed.
- **Hooks:** the event and body rules, an http body's url and headers (no
  `Host`, `Content-Type` or other header the call sets itself, no line
  break in a value), and a code-js body only when this host has code
  hooks enabled, compiled here.
- **Channels:** the name, scope and semantic a create accepts, no name a
  yaml channel here already holds, and hooks whose HookDef references
  resolve on this host.
- **Agents and teams:** their inline hook webhooks, by the same header
  and url rules. **Skills:** a non-blank body.

This can skip a definition an older restore brought back — typically an
MCP server whose host this host does not allow. Add the host to the
allowlist and restore again: the rows already restored are left alone.

`paused_runs` reference session_ids, but sessions aren't a
captured section. Restore synthesizes a session row deterministically
(`snap_sess_<run_id>`) before inserting the run; the
`synthesized_sessions` counter on the response surfaces this so
operators can audit cross-instance restore behaviour.

### Schedules

The `schedule_defs` section carries every schedule definition with its
run state (`next_run_at`, last run, pause, `fire_count`), and
`schedule_def_active` carries the active pointers. A restored schedule
is **active**: a `next_run_at` in the past fires once on the first
sweep after resume, and `fire_count` is restored as it was, so a
`max_fires` schedule keeps only the fires it had left. A schedule, a
pointer or a run state already on the target stands; a re-restore never
resets a count.

Literal `user_credentials` values are never written into a snapshot.
Their keys are listed on the entry as `stripped_credentials`, and the
schedule is restored **disabled** with a `capture_disabled` marker. A
warning names it and its keys, and `defs_disabled_for_credentials`
counts it. Re-enable it with a ScheduleDef `fork` that supplies every
listed key and `enabled: true`; the fork keeps the fire count already
spent. A `create` on the same name counts as a new version: it keeps the
marker unless it supplies every key, and keeps the fire count. References (`$cred:<name>`, `${...}`, `user_credentials_from_env`
names) travel as written.

**A snapshot is a copy, not a lease.** Restoring into a second instance
while the first keeps running fires every enabled schedule on both, and
together they can exceed `max_fires`. Nothing in the runtime prevents
it: disable the schedules on one side. Every restore that brings back
an enabled schedule warns once with the count.

### Webhooks and A2A definitions

The `webhook_defs`, `a2a_agent_defs` and `a2a_server_card_defs`
sections carry every definition with its active pointers
(`webhook_def_active`, `a2a_agent_def_active`,
`a2a_server_card_def_active`). A definition already on the target
stands, including one the target bootstrapped from its own yaml: a
collision on the same name and version is a warning, not an overwrite.

A webhook's literal `user_credentials` values are stripped exactly as a
schedule's are. The webhook is restored **disabled** with a
`capture_disabled` marker listing the stripped keys, and it answers
every delivery with `404` — even if its body says `enabled: true` — until
a WebhookDef `fork` whose own overlay supplies **every** listed key (in
`user_credentials` or `user_credentials_from_env`) and sets
`enabled: true`. A key the parent already sources does not count. A fork
that supplies only some of them stays disabled, and its marker lists the
keys still missing. A `create` on the same name follows the same rules.
It also counts in `defs_disabled_for_credentials`.
The signing-secret and bearer env-var names travel as written.

A2A definitions carry no secret: a peer's `auth.bearer_credential_ref`
names a per-run credential key, and a card holds public scheme
descriptions and an env-var name. A restored active server card
publishes an AgentCard and accepts A2A calls for the agents it exposes
once the runtime resumes, and the restore warns once per card.

**Every restored webhook, A2A peer and server card is re-validated** with
the rules an author faces on this host: auth kind and env-var-name
charset, delivery-mode rules, peer endpoint and agent-card URL checks
(http(s) only, no literal private or metadata address for gRPC). A
definition that fails is not restored, and neither is a pointer at it;
the warning names it and the reason.

### Memory backends and document sources

The `memory_backend_defs` and `document_source_defs` sections carry every
definition with its active pointers (`memory_backend_def_active`,
`document_source_def_active`), restored before memory so a run resumed by
the same restore routes its Memory and Document calls through them. A
definition already on the target stands, as above. A restored definition
is live at once: nothing needs a restart.

`config.api_key_env` and `tenancy_strategy.env_pattern` are env-var
**names**. They travel as written and are never resolved, so the key's
value never enters the snapshot. But the host sends that key to
`config.base_url`, so **every restored memory backend and document
source is re-validated** with the rules an author faces: `base_url` must
be an http(s) URL with a host, and `api_key_env` must be an allowed
credential name, never one of loomcycle's own secrets such as
`LOOMCYCLE_AUTH_TOKEN`. A peer that dials (a remote backend, every
source) must also be at a host **this host** lists in
`LOOMCYCLE_HTTP_HOST_ALLOWLIST` or `LOOMCYCLE_HTTP_PRIVATE_HOST_ALLOWLIST`,
unless its `base_url` is exactly the one this host's yaml declares under
the same name. A definition that fails is not restored, and
neither is a pointer at it. An old `kind: mem9` backend is refused the
same way, since authoring refuses it.

### Dynamic volumes

The `volume_defs` section carries each persistent dynamic volume's
tenant, name and mode. It never carries the volume's host path: the path
a volume resolves to is what the file and exec tools are confined to, so
restore **derives it on the target**, under the target's own
`dynamic_root`, exactly as `VolumeDef create` would, and creates the
directory there (`0700`). A path added to a snapshot by hand is ignored.

- **Contents are not carried.** A restored volume is an empty directory;
  each one produces a warning saying so. If a directory already sits at
  the derived location, it is bound as found and the warning says that
  instead. Copy the files across yourself.
- **Skipped, with a warning:** the target has no `dynamic_root` (the whole
  section, once); the name is a static `volumes:` entry on the target,
  which would take precedence anyway; the name, mode or tenant fails the
  checks `create` applies; or the directory cannot be created (including
  when a symlink sits where it would go). A volume is only recorded once
  its directory exists.
- **The live volume stands.** A volume the target already has keeps its
  mode and its directory; if the snapshot's differs, a warning says how.
- Restored volumes count in `volume_defs`; the directories the restore
  created count in `volume_dirs_created`.
- Warnings name a volume's location relative to the dynamic root
  (`<dynamic root>/acme/data`), never the host path.
- Ephemeral (run-scoped) volumes are not carried.

### Queued memory (not yet consolidated)

`Memory op=add` (and a compaction banking what it discards) queues
conversation turns for background consolidation. The `memory_pending`
section carries every queued item that has **not been consolidated
yet**, with its tenant, scope, user or agent, text, origin and source
run. Items already consolidated are not carried: their facts are in the
`memory` section.

- **Always captured, paused or not.** The queue is read before
  `memory`. A pass that finishes between the two reads leaves the item
  in both, so the target consolidates it once more — a duplicate, never
  a loss. Repeats usually merge into the facts the first pass wrote, but
  extraction is model-driven, so a near-duplicate fact can remain. Pause
  first for an exact copy.
- **Consolidated on the target.** Items land unconsolidated and
  unclaimed: the source's consolidation lease and progress markers do
  not travel. The target's scheduled consolidation finds a restored
  queue even for a user with no chats on the target.
- **The target's own queue stands.** An item already on the target, even
  one it has consolidated since, is left alone, so restoring the same
  snapshot twice does not queue anything twice.
- Restored items count in `memory_pending`.
- An erasure of a subject on the target removes their restored items,
  as it removes their memory. Restoring a snapshot taken before an
  erasure brings them back, as it brings back everything else.

### Path names

The `dirents` section carries the Path tree: every name a document, a
memory entry or a volume mount is found by, in every tenant's agent,
user and tenant trees. A restored document is therefore reachable at the
path it had (`Document op=get_document path:…`) and listed by
`Path op=ls`, not only by its id. A document named twice keeps both
names.

- **Restored last**, after the documents, memory and volumes the names
  point at. Each name keeps its tenant, scope and user or agent exactly,
  so a name never lands in another tree.
- **Never a name pointing at nothing.** A name whose document, memory
  entry or volume is not on the target is not restored. The skipped
  names produce one warning per tree and kind, with a count and the first
  few paths. The usual cause is the SQL Memory section not restoring (SQL
  Memory is off on the target, or the tiers differ), which leaves every
  document without its structure. Restoring the same snapshot again once
  the documents can land brings the names back.
- **Directories come back with their contents.** A directory exists
  because something is named under it, so `ls` of every ancestor lists
  it again. An empty folder made with `Path op=mkdir` is carried as it
  is.
- **The target's own names stand.** A path already taken on the target
  is left alone. If it names something else, a warning says so. If it
  names the same thing, as on a second restore, nothing is said.
- Every name is re-checked with the rules the Path tool writes by (one
  `[a-zA-Z0-9._-]` segment per level, no `..`, a known scope and kind).
  A name that fails is not restored, and the warning names it.
- Restored names count in `dirents`.

### A paused run waiting on a question

Interrupts are not carried. A run asks a question from inside a tool
call, and a pause only parks a run between steps, so a paused run is
never waiting on one. Capture checks this for every paused run it
includes. If one does have a pending interrupt, the capture still
succeeds, but it warns and names the run: in the capture response, in
the log, and in the snapshot itself, and every restore of that snapshot
repeats the warning. Resolve or cancel the interrupt, then capture again.

### Restore warnings and missing credentials

A restore warning that quotes a URL has the URL's user/password, query
and fragment replaced by `REDACTED`, so a refused endpoint's embedded
credential is never printed.

**Literal credentials.** A capture reports every header value, and every
value in an MCP server's stdio `env`, that looks like a literal
credential instead of a reference (`$cred:<name>`, `$ghapp:<name>`,
`${...}`). The value travels in the snapshot as written. The capture
response and every restore warn with its location — the definition and
the field, such as `headers.Authorization` or `env.GITHUB_TOKEN` — never
the value. Replace it with a reference; in a stdio `env`, only a
`${LOOMCYCLE_*}` one resolves.

**Missing credentials.** A snapshot never lists the source's
credentials. After the definitions land, the restore reads the
references in each restored webhook, schedule and server card —
`user_credentials_from_env` names, the webhook signing/bearer env names,
`sign_with_key_env`, and `$cred:` / `$ghapp:` / `${...}` references —
and in the header maps of every restored MCP server, hook, channel,
agent and team (plus an MCP server's url and stdio `env`), and warns for
each one this host cannot supply: an env var that is not set, or a
credential the definition's tenant does not have. A warning names the
definition and the reference, never a value.

For a memory backend that dials a peer (`kind: remote`) and for every
document source, the restore checks the env var a call would send: a
tenant's own `key_per_tenant` pattern with its tenant filled in, else
`api_key_env`. An operator-layer pattern names a different variable for
each calling tenant, so it is not checked; an in-process backend sends
no key, so nothing it names is checked.

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

- **Not a backup.** Snapshot scope is running-state only (the
  sections listed above). External DB
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

## Pair with the History tool

Restored experiments contain interaction history events
(opt-in via `include_history=true`). Agents in the restored
instance read past chats via the `History` tool
(`History op=get session_id:<id>` for a transcript, or
`op=list` to browse) to reflect on prior conversations.
Self-evolving experiments survive cross-version migration with
their memory intact.

## State-change signals

State transitions (`running → pausing → paused → running`) publish
to the operator-declared `_system/runtime-state` channel (v0.8.6
system channels). Operator dashboards and external orchestrators
subscribe to that channel via the existing Channel interface —
no new SSE event types were added for pause / resume.
