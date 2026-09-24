---
name: Agent/spawn
description: "Agent op=spawn — run one registered sub-agent on a task and wait for its final answer."
---
`spawn` hands one task to one sub-agent and returns its final reply when it
finishes. It is the default: a call with no `op` is a `spawn`. **The `prompt`
is everything the child knows about the task** — it does not see your
conversation — so put all it needs in it.

## Arguments

- `name` (required) — a registered agent name (`Context {"op":"agents"}`).
- `prompt` (required) — the task, complete. No tokens or keys: the child gets
  its own credentials.
- `def_id` — run a specific version of that agent (from `AgentDef`). The
  version's name must match `name`.
- `compaction` — override the child's context compaction (it inherits yours):
  `enabled`, `target_percentage` (10–50), `keep_last_n`, `keep_first`,
  `autocompact_at_pct` (50–95), `model`.

Do not pass `spawns` here; that is `parallel_spawn`.

## Returns

The child's final text, as plain text, starting with a line
`[sub-agent agent_id=a_...]`. A child that keeps structured state adds
`Final state:` and its JSON.

## Errors

- `missing required field: name` / `prompt` — add it.
- `unknown sub-agent "X" ...` — no agent is registered under that name for
  your tenant. Check `Context {"op":"agents"}`; retrying is pointless.
- `Agent tool: def_id "..." is for agent "Y", not "X" ...` or `... is
  retired` — use a `def_id` that belongs to `name` and is live, or omit it.
- `op=spawn must not carry a 'spawns' array` — you mixed in the
  `parallel_spawn` shape.
- `sub-agent "X" failed (...)` — the child ran and failed. The message says
  why; decide whether to retry with a clearer prompt.
- `max sub-agent recursion depth (3) reached ...` — you are too deep to
  delegate; do the work yourself.

## Examples

Delegate one task and wait for the answer:

```json
{"op": "spawn", "name": "cv-adapter", "prompt": "Write a one-page CV for candidate c_4471 targeting a senior backend role. Profile: 8 years Go, Postgres, Kubernetes; led a team of five."}
```

```json result
"[sub-agent agent_id=a_9f2c41d07be35a18]\n# Jane Doe — Senior Backend Engineer\n..."
```

The same, with a child that should compact its context early on a long task:

```json
{"op": "spawn", "name": "researcher", "prompt": "Survey the 2026 literature on retrieval-augmented agents and list the ten most cited papers with one line each.", "compaction": {"enabled": true, "autocompact_at_pct": 70}}
```
