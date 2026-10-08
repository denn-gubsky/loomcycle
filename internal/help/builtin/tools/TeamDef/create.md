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
  optionally `max_iterations`, `colors`, `layout`, `channels`, `hooks`,
  `local`, `vars`.
  Keys are read exactly as written, at every depth: a key the definition
  does not have, a key in another case (`"Hooks"` for `hooks`) and a key
  written twice in one object are each refused, with the key's path.
- `description` — why this version exists. Beside `overlay`, not inside it.
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
- `create: <object>: unknown key "X"` / `key "X" is read as "x" only by
  ignoring its case` / `key "x" is written twice` / `keys "x" and "X" are both
  read as "x"` — the overlay would be read differently from its text. Write
  each key once, spelt as the definition spells it. `verify` with the same
  overlay lists every such key with its path.

## Examples

A two-state team: one reviewer, then done.

```json
{"op": "create", "name": "pr-review", "description": "single reviewer",
 "overlay": {"entry": "review",
  "states": [{"state": "review", "handler": {"kind": "agent", "agent": "reviewer"}},
             {"state": "done", "handler": {"kind": "terminal"}}],
  "transitions": [{"from": "review", "to": "done", "on": "success"}]}}
```
