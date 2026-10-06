---
name: TeamDef/run
description: "TeamDef op=run — walk a team's workflow for an input: wait for its answer, run it in the background as a child of your run (mode poll), or hand it off (mode detach)."
---
`run` walks a team's graph for one input, state by state, until it reaches a
terminal state, and answers with the trace. The walk is a run of its own, with
a `run_id`. See `help(topic="agent-teams")` for what a team is and how each
state runs.

It has three modes:

- **Wait** (default, no `mode`): the call returns when the walk is over, with
  its whole answer.
- **Poll** (`mode: "poll"`, inside an agent's run): the call returns
  `{run_id, state: "running"}` at once and the walk runs as a background child
  of your run. You keep working; a note on your next turn says when it ended;
  read its answer with `poll`, or end it early with `cancel`. Your run does
  not end while the walk runs — ending your turn waits for it — and the walk
  is cancelled if your run is.
  It counts against your live children, and is refused on your last
  iteration.
- **Detach** (`mode: "detach"`): the call returns `{run_id, status: "running"}`
  at once and the walk runs on outside your run — not your child, never
  reported to you, not cancelled with you. For handing a walk to an operator.

## Arguments

- `name` — the team; its active version runs. Or `def_id`.
- `def_id` — run one version of a team instead of its active one.
- `input` — the task the entry state works on. Checked against the team's
  input form, if it has one.
- `vars` — values for variables the team declares, name → text. A name the
  team does not declare is refused before anything runs.
- `mode` — `poll` or `detach`; omit to wait.
- `notify` — poll mode only. `true` (default): a note on your next turn when
  the walk ends. `false`: no note; you poll.
- `on_parent_end` — poll mode only. `wait` (default): ending your turn waits
  for the walk. `cancel`: the walk is cancelled when you end your turn.
- `board_chunk_id`, `board_scope` — bind the walk to a Document chunk: each
  state it enters is written to the chunk's status, and a later run resumes
  from it.
- `interrupt_on_cap` — when a state hits its iteration cap, ask a person
  whether to continue, reroute or abort, instead of stopping.
- `breakpoints` — starter states to pause at before they dispatch; each pause
  asks a person to release or abort.
- `review`, `review_ttl_seconds` — states whose member runs are held for a
  person's verdict when they finish, and the deadline on each hold.

## Returns

Wait mode: `{name, def_id, run_id, status: "completed", final_state,
final_output, steps: [{state, agent, edge, next, output}]}` — `final_output`
is the last state's output. A walk stopped by an iteration cap answers
`status: "iteration_cap"` with `capped_state` and the steps so far.

Poll mode: `{name, def_id, run_id, state: "running"}`. Read the answer above
with `poll` once the walk has ended. A person holding the walk — a breakpoint
or cap question, or a member held for review — shows it as `held`.

Detach mode: `{name, def_id, run_id, status: "running"}`.

## Errors

- `run: team not found` — no active version of that name in your tenant.
- `run: unknown mode ...` — `mode` is `poll` or `detach`, or omitted.
- `mode "poll" is not available in this run` — poll mode needs an agent's run
  to be a child of; from outside one, wait or detach.
- `mode "poll" is refused on your last iteration ...` — no turn would be left
  to read the walk; omit `mode`.
- `this run has N children alive and may have at most M at once ...` — wait
  for children you started to end, then retry.
- `notify and on_parent_end apply to mode "poll" only` — add
  `"mode": "poll"` or drop them.
- `run: ...` — the walk failed; the message says at which state and why.

## Examples

Run a team in the background, keep working, and read it later:

```json
{"op": "run", "name": "pr-review", "input": "Review PR #412", "mode": "poll"}
```

```json result
{"name": "pr-review", "def_id": "tdf_9a1c3e5b7d2f4086", "run_id": "r_6f2a9c1e4b8d7035", "state": "running"}
```

Wait for a walk, with one of its variables set:

```json
{"op": "run", "name": "pr-review", "input": "Review PR #412", "vars": {"tone": "terse"}}
```
