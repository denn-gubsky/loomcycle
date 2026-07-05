---
name: interruption
description: Human-in-the-loop primitive (v0.8.16). Ask / notify / cancel ops with three delivery surfaces (webui / consumer-MCP / cli) and forward-compatible kind enum for future pause / wait_until / approval.
---
v0.8.16 added the **Interruption** tool — the human bridge in the
loomcycle substrate. Where Memory persists state, Channel routes
inter-agent messages, AgentDef versions definitions, and Evaluation
records scores, Interruption surfaces questions to humans and
blocks the agent loop until they answer.

## Three ops

- **`ask`** — surface a question to a human. The agent loop blocks
  on the answer. Result text is the human's response, ready to
  feed into the next assistant turn.

  ```json
  {"op": "ask",
   "question": "Should I proceed with deleting the 47 draft records?",
   "options": ["Yes, proceed", "No, abort"],
   "context": "Records have no activity since Jan 2025.",
   "timeout_ms": 3600000,
   "priority": "normal"}
  ```

- **`notify`** — fire-and-forget message (no answer expected, no
  block). Surfaces to operators via the same delivery surface as
  ask but doesn't carry an answer back.

  ```json
  {"op": "notify", "message": "Batch job complete: 47 records deleted."}
  ```

- **`cancel`** — agent withdraws a previously-asked question (e.g.
  it figured the answer out from later context). The pending row
  transitions to status=cancelled.

  ```json
  {"op": "cancel", "interruption_id": "intr_..."}
  ```

## Three delivery surfaces

The agent-facing tool surface is identical across surfaces; the
operator picks which one renders the question to a human.

- **`webui` (default)** — humans answer via the embedded
  `/ui/interrupts` inbox. Production posture.
- **`mcp_server:<name>`** — loomcycle calls the consumer's
  `mcp__<name>__ask` MCP tool. The consumer's tool blocks
  (HTTP round-trip → human → answer); the result becomes the
  ask's answer. Useful when the consumer already runs an MCP
  server and wants to render questions in its own UI.
- **`cli`** — local-dev only; an external script subscribes to
  `_system/interrupts/pending` and posts the resolve endpoint.

A 21st LoomCycle MCP meta-tool (`interruption_resolve`) lets
external orchestrators (Claude Code, custom dashboards) act as
the answerer regardless of which backend is configured.

## Per-agent ACL

Default-deny. Operator yaml grants:

```yaml
agents:
  batch-processor:
    tools: [Interruption, ...]
    interruption:
      enabled: true
      kinds: [question]    # v0.8.16 only valid value
      max_pending: 1       # 0 = use operator default
```

Without the `interruption` block, every op returns
`is_error: true` with a clear "not enabled" refusal — same
default-deny shape as memory_scopes / channels /
agent_def_scopes / evaluation_scopes.

## Forward-compatible kind

The storage `kind` column is a **closed enum** owned by loomcycle.
v0.8.16 writes only `kind: question`. Future kinds slot in
additively without reopening the design:

- `pause` — block for any reason (debug step-through, manual
  review). Resolve body carries no answer.
- `wait_until` — scheduled-wake. Server-side timer auto-resolves
  at the target timestamp.
- `approval` — yes/no boolean shortcut with `answer_meta:
  {approved, reason}`.

The `_system/interrupts/*` channel namespace + the
`interrupts.kind` column + the resolve endpoint's `kind`-
discriminated body are the forward-compat seams.

## Blocking + heartbeat

`ask` blocks via `channels.Bus.Wait` on a dedicated bus key
(`intr:<interrupt_id>`). The resolve HTTP handler writes the
row + calls `bus.Notify`; the wait wakes.

A dedicated heartbeat ticker fires
`Store.UpdateHeartbeat(run_id)` every
`LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS` (default 30s)
while blocked. Without this, the v0.5.0 sweeper (default
`StaleAfter` 10 min) would reap long-pending interrupts as
crashed runs.

## What the model never sees

- The bearer for the resolve endpoint (cookie-authed Web UI;
  bearer-authed admin path)
- The `resolved_by` attribution (server-derived from cookie vs
  bearer)
- The Web UI consumer's identity
- Other users' pending interrupts (queries filtered by
  denormalised `user_id`)

## When NOT to use Interruption

- Non-interactive batch jobs — no human is watching. Use
  hardcoded policy + Memory for state instead.
- For routing decisions the model already has data for —
  ask only when the human's input is genuinely necessary.
- For cross-agent coordination — use Channel for agent-to-agent;
  Interruption is human-to-agent only.

## References

- `docs/TOOLS.md` "The Interruption tool" section
- `doc-internal/rfcs/interruption-tool.md` (internal RFC)
- `internal/tools/builtin/interruption.go`
- v0.8.16 release notes in `REVISIONS.md`
