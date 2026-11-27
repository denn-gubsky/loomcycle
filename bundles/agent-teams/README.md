# agent-teams bundle

Agents for **building** and **running** agent teams (RFC AP + RFC BD), plus the
`team/*` skills they use.

| Agent | What it does |
|-------|--------------|
| `agent/assistant` | Helps you design + author a specialized **AgentDef** — the reusable building block a team routes between (role, least-privilege tools, tier, skills, scopes). |
| `team/assistant`  | Assembles a **TeamDef** (a workflow state machine) from agents that *already exist*. Renders the Mermaid diagram, gets your sign-off, commits, and can `TeamDef op=run` to test. Does **not** create agents — points you at `agent/assistant`. |
| `team/orchestrator` | **(RFC BD) the LLM team lead + human contact point.** An interactive, steerable run that *drives* a team: reads the TeamDef as its map, moves a Document task board through the states, spawns each state's handler agent, decides routing, and — for software teams — opens a git workspace on the repository + opens a PR. |

`team/orchestrator` complements the deterministic **`TeamDef op=run`** autopilot:
op=run is headless/linear and code-driven; the orchestrator is intelligent,
human-in-the-loop, and domain-general.

## Select it

```
LOOMCYCLE_PRESETS=base,agent-teams
```

The bundle carries **no provider matrix** — agents declare `tier: middle`, so
select `base` (or your own config) alongside it to supply a `middle` tier. Run
`team/orchestrator` **interactively** (start it with `interactive:true` from the
`/run` terminal) so it's your steerable contact point.

### Software teams only (repo/PR)

For a team whose deliverable is a code change (the `team/repo` skill), the
orchestrator works on a git workspace (RFC GW P11, P14): it opens the
repository, its handlers edit it through a share, and it commits and pushes the
workspace's own branch. Set:
`workspace.mode` on (a worker with git), the forge's host in
`LOOMCYCLE_GIT_SOURCE_HOSTS` and `LOOMCYCLE_GIT_PUSH_HOSTS`, and a stored
credential for the repository (`$cred:<name>`). `team/orchestrator` holds
`git_remote: [fetch, push]`. To open the pull request through the forge's API,
add an egress route with a credential (e.g. `github-api`) to the orchestrator's
workspace grant; without one it reports the pushed branch. Marketing/accounting/research
teams need none of this — the workspace is domain-pluggable (Documents, SQL
Memory, or external SQL/Excel in a workspace or through an MCP tool).

Where workspaces are off, a software team cannot run: the Bashbox host-command
fallback `team/repo` had was removed in v1.172.0 with Bashbox
(`docs/MIGRATING-FROM-BASH.md`).

`team/orchestrator` also holds `Workspace` and a workspace grant (read, write,
exec, open; RFC GW P14): it runs a data team's scripts in a workspace, and the
handlers it spawns open their own workspaces under it.

## What's inside

- `loomcycle.yaml` — the bundle config: the three agents + the inline
  `team/structure`, `team/workflow`, `team/orchestrate`, and `team/repo` skills.
  This mirrors the embedded copy at
  `internal/embedded/bundles/agent-teams.yaml` (the one the binary ships);
  keep the two in sync.

## The workflow model (RFC AP)

A `TeamDef`'s definition is a **state-machine graph**: states (nodes) with a
handler each — `agent` | `parallel`+consolidator | `consolidator` | `terminal` —
and transitions (edges) gated by `success` / `pushback:<reason>` /
`conditional:<expr>`. The graph is validated before any write; loops are bounded
by a per-state `max_iterations` cap. Colours are presentation-only (excluded from
the content hash) and drive the generated Mermaid diagram.

`op=run` walks a team end to end and executes every handler kind — a single
`agent`, a `parallel` fan-out (agents run concurrently; `wait` = `all` | `any` |
`at_least:<N>`), and a `consolidator` that reads the results and selects the
outgoing edge, so `pushback` rework loops route exactly as they render. A
consolidator names its edge on a `signal: <edge>` line (last one wins; no signal
defaults to `success`).
