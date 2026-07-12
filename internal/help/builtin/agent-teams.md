---
name: agent-teams
description: Agent teams — a TeamDef is a state-machine workflow (states + transitions + a per-state handler agent) that a team walks task-by-task, driven either by the deterministic op=run autopilot or by an LLM team/orchestrator.
aliases: [teamdef, teams, team]
---
An **agent team** is a workflow plus the agents that carry it out, captured as a
`TeamDef`. The definition is a **state-machine graph** — the graph *is* the
workflow, and it is domain-agnostic (software delivery, marketing, accounting,
research, …).

## The model

- **States** are the steps a unit of work passes through (e.g. `architecture` →
  `implementation` → `review` → `pr`). Each state binds a **handler**:
  - `agent` — one agent runs the step;
  - `parallel` — several agents fan out, then a `consolidator` agent reads their
    outputs and picks the outgoing edge;
  - `consolidator` — a standalone judging step;
  - `terminal` — an end state (no agent, no outgoing edges).
- **Transitions** are the edges between states, gated by an `on` label:
  `success` (advance), `pushback:<reason>` (loop back for rework), or
  `conditional:<expr>`. A state's outbound labels are unique, and every cycle is
  bounded by a per-state `max_iterations` cap so a workflow always terminates.

## The task board

Live work rides on a **Document** used as a task board: one chunk per work-item,
and the chunk's `status` field holds its current state. The team advances a chunk
by moving `status` from one state to the next per the transitions. (See the
`document` help topic.)

## Two ways to run a team

- **`TeamDef op=run`** — the deterministic **autopilot**: it walks a team's graph
  end to end and returns the per-state trace. Headless, no human in the loop. It
  executes every handler kind — a single `agent`, a `parallel` fan-out (its agents
  run concurrently; `wait` = `all` | `any` | `at_least:<N>`), and a `consolidator`
  that reads the results and picks the outgoing edge, so `pushback` rework loops
  route just as they render. A consolidator selects its edge by emitting a line
  `signal: <edge>` (e.g. `signal: success` or `signal: pushback:redo`): the
  `<edge>` must match one of the state's outbound transition labels, the last such
  line wins, and no signal defaults to `success`. Every cycle is bounded by the
  per-state `max_iterations` cap.
- **The `team/orchestrator` agent** — an LLM **team lead** and the human's
  contact point. Run it interactively: it reads the TeamDef as its map, drives
  the Document board (moving `status`, spawning each state's handler), decides
  routing (including pushback + parallel fan-out via the Agent tool), and — for
  software teams — sets up an ephemeral repo volume and opens a PR. It keeps the
  human in the loop and is steerable mid-run.

## Authoring + running

- **Build** a team with the `team/assistant` agent (it assembles a TeamDef from
  agents that already exist) or the `TeamDef` tool directly
  (`op=create` / `fork` / `promote` / `render_diagram`). The graph is validated
  before any write — a dangling transition, unreachable state, or
  parallel-without-consolidator is refused.
- **Inspect** a team's shape with `TeamDef op=render_diagram` (a Mermaid
  `stateDiagram-v2` with the colour scheme applied).
- **Handlers** are ordinary agents; a team just names them per state. Missing a
  role? Build it with the `agent/assistant` agent first.

## Cross-references

- `help(topic="document")` — the chunked-graph Document used as the task board.
- `help(topic="subagents")` — how handler agents are spawned (spawn vs parallel).
- `help(topic="volumes")` / `help(topic="volumedef")` — the workspace a software team clones a repo into.
- `Context op=permissions` — your effective tools + scopes.
