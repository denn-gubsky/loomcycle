---
name: subagents
description: When to spawn a sub-agent via the Agent tool vs publish to a channel; def_id pinning; depth limits.
---
You can collaborate with other agents two ways: **synchronous spawn**
(the `Agent` tool — call N, wait, get N's final text back) or
**asynchronous handoff** (publish to a `Channel`, the other agent
subscribes when ready). They serve different goals.

## When to use Agent (sync spawn)

You want the sub-agent's OUTPUT before you continue:

- Coordinator delegates a specialised task ("write the CV from this
  candidate profile") and waits for the result.
- Parent needs the structured response to feed into its next step.
- Sub-agent's session and transcript are persisted independently;
  you only see its final assistant text.

```
{"name": "cv-adapter", "prompt": "Generate CV for ..."}
→ "[sub-agent agent_id=a_abc]\n<sub-agent's final output>"
```

The sub-agent's own ACL applies — your tool set doesn't transfer.
Each agent definition is operator-curated and self-describing.

## `Agent` (in-loop) vs `spawn_run` (MCP surface)

These live at different layers and are easy to confuse:

- **`Agent`** is a *built-in tool* you call from **inside** a run.
  It spawns a sub-agent as a child of your current run (parent/child
  identity, cancel-cascade, depth cap — all above). You are already an
  agent; `Agent` is how you delegate mid-loop.
- **`spawn_run`** / **`register_agent`** / **`list_agents`** are
  *MCP meta-tools* an **external** orchestrator (Claude Code, Claude
  Desktop, a custom client driving `loomcycle mcp`) calls to start a
  **top-level** run and manage the agent registry from outside the
  runtime — the same surface as the HTTP `POST /v1/runs` connector.

Rule of thumb: if you're an agent already running, you use `Agent`;
if you're a client launching loomcycle work from the outside, you use
`spawn_run`. They are not alternatives to choose between — they're the
inside and outside views of the same run machinery.

## When to use Channel (async handoff)

You don't need an immediate result:

- Producer drops work items for a worker pool to drain when ready.
- Fan-out broadcast where many subscribers act independently.
- Long-running pipeline where stages don't block each other.
- Operator-driven coordination (an admin endpoint publishes alarms;
  agents subscribe and react).

```
{"op":"publish","channel":"findings","value":{...}}
→ persists immediately; subscriber pulls when ready
```

## Recursion depth cap

Sub-agents can spawn sub-sub-agents, but loomcycle caps recursion
at depth 3 (top-level run = 0, its children = 1, grandchildren = 2,
attempting depth 3 refuses). Designed to bound runaway self-spawning
prompt loops.

## def_id pinning (v0.8.5 substrate)

The `Agent` tool accepts an optional `def_id` — pins the sub-run to
a specific `agent_defs` row instead of resolving the currently-active
def. Use this for **experimentation**:

```
{"name":"reviewer", "prompt":"Review this PR", "def_id":"def_abc123"}
```

The sub-agent runs against the pinned def's system prompt and
allowed_tools (within the operator's static ceiling). Useful for
A/B-testing forks without flipping the live active pointer. See
`help(topic="experimentation")` for the full pattern.

## Cross-name pinning is refused

If you pass a `def_id` whose row was created for a different agent
name, the runtime refuses with "cross-name pinning refused". This
prevents a parent from hijacking another agent's namespace.

## Sub-agent identity flow

The parent's `agent_id` becomes the sub-run's `parent_agent_id`.
Cancellation cascades: if the parent run is cancelled, every active
sub-run in its tree gets cancelled too. The `user_id` and `user_tier`
inherit from the parent.

## Quick rule of thumb

| You're doing | Use |
|---|---|
| "Write me this thing, I'll wait" | Agent |
| "Drop this on the queue; analyst will pick it up" | Channel |
| "Run 5 forks of myself in parallel and aggregate" | Agent (with def_id) |
| "Notify everyone subscribed to alerts" | Channel (broadcast) |
| "Run this side task in the background" | Channel (async worker) |
