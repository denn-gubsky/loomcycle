---
name: TeamDef
description: "TeamDef tool — author, version and inspect team workflows (a graph of states, each run by agents), and run one: wait for it, run it in the background as your child, or hand it off."
---
The `TeamDef` tool manages **teams**: workflow graphs whose states are run by
agents, with transitions that say where the work goes next (`success`,
`pushback`, a condition). It authors and versions them, and runs one for an
input — a **walk** — through the sub-agent machinery. For what a team is, how
each kind of state runs and how to design one, read
`{"op":"help","topic":"agent-teams"}`.

## When to use it — and when not

- **Use TeamDef** to build a team, change one (a fork is a new version), or run
  one whose graph already does the job.
- **Use `Agent`** instead for one sub-agent, or a fan-out you decide on the
  spot: a team is for a workflow worth naming and reusing.

## Operations

Fetch one operation's article, with examples, as `TeamDef/<op>` — for example
`{"op":"help","topic":"TeamDef/run"}`.

| op | What it does | Required besides `op` |
|---|---|---|
| `create` | store the first version of a new team (promoted by default) | `name`, `overlay` |
| `fork` | store a new version of a team, over its active version | `name` |
| `get` | one version, with its definition | `def_id` |
| `list` | every version of a team | `name` |
| `retire` | retire or un-retire one version | `def_id`, `retired` |
| `delete` | remove a whole team, every version | `name` |
| `promote` | make one version the active one | `def_id` |
| `verify` | compare a hash with the active version, or check an unsaved draft | `name` |
| `render_diagram` | a Mermaid diagram of a stored team or a draft | `name` or `def_id` or `overlay` |
| `run` | walk a team for an input: wait, poll mode or detach | `name` or `def_id` |
| `poll` | read walks you ran in poll mode | — |
| `cancel` | end walks you ran in poll mode | `run_ids` |

## Versions

Every save is a new, immutable version with its own `def_id`; a team's name
points at one **active** version, which `run` uses unless you pin a `def_id`.
`create` makes it active; `fork` does not unless you pass `promote: true`.
Teams belong to your tenant: another tenant's are invisible, and asking for
one by `def_id` is an ordinary "not found".
