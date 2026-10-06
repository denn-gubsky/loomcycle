---
name: TeamDef/create
description: "TeamDef op=create — store the first version of a new team from its workflow graph; it becomes the active version unless promote is false."
---
`create` stores a new team: a name and its graph. The graph is checked before
anything is written — a dangling transition, an unreachable state, a parallel
state without a consolidator, a channel the team cannot reach — and a graph
that fails is refused with nothing stored. `verify` with the same overlay runs
these checks without saving and reports every problem at once.

For the graph's shape (states, handlers, transitions, `local`, `vars`) read
`{"op":"help","topic":"agent-teams"}`.

## Arguments

- `name` (required) — the new team's name: one segment of `A-Z a-z 0-9 _ -`,
  at most 64 characters.
- `overlay` (required) — the graph: `entry`, `states`, `transitions`, and
  optionally `max_iterations`, `colors`, `hooks`, `local`, `vars`.
- `description` — why this version exists.
- `promote` — `false` stores the version without making it active (default
  `true`).

## Returns

The stored version: `{def_id, name, version, parent_def_id, description,
created_at, created_by_agent_id, retired, content_sha256, definition,
promoted}`.

## Errors

- `create: missing required field: name`.
- `create: ...` — the graph or the name was refused; the message names the
  problem and where it is.

## Examples

A two-state team: one reviewer, then done.

```json
{"op": "create", "name": "pr-review", "description": "single reviewer",
 "overlay": {"entry": "review",
  "states": [{"state": "review", "handler": {"kind": "agent", "agent": "reviewer"}},
             {"state": "done", "handler": {"kind": "terminal"}}],
  "transitions": [{"from": "review", "to": "done", "on": "success"}]}}
```
