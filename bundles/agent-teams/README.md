# agent-teams bundle

Agents for **building** and **running** agent teams (RFC AP + RFC BD), plus the
`team/*` skills they use.

| Agent | What it does |
|-------|--------------|
| `agent/assistant` | Helps you design + author a specialized **AgentDef** — the reusable building block a team routes between (role, least-privilege tools, tier, skills, scopes). |
| `team/assistant`  | Assembles a **TeamDef** (a workflow state machine) from agents that *already exist*. Renders the Mermaid diagram, gets your sign-off, commits, and can `TeamDef op=run` to test. Does **not** create agents — points you at `agent/assistant`. |
| `team/orchestrator` | **(RFC BD) the LLM team lead + human contact point.** An interactive, steerable run that *drives* a team: reads the TeamDef as its map, moves a Document task board through the states, spawns each state's handler agent, decides routing, and — for software teams — sets up an ephemeral repo volume + opens a PR. |

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

For a team whose deliverable is a code change (the `team/repo` skill), also set:
`LOOMCYCLE_BASHBOX_ENABLED=1`, `git`+`gh` on the host image,
`LOOMCYCLE_BASHBOX_FALLBACK_COMMANDS=git,gh`,
`LOOMCYCLE_BASHBOX_FALLBACK_ALLOWED_ENV=GITHUB_TOKEN` (+ `GITHUB_TOKEN` in the host
env), and a volume marked `dynamic_root: true`. Marketing/accounting/research
teams need none of this — the workspace is domain-pluggable (Documents, SQL
Memory, or external SQL/Excel via Bashbox/MCP).

## What's inside

- `loomcycle.yaml` — the bundle config: the three agents + the inline
  `team/structure`, `team/workflow`, `team/orchestrate`, and `team/repo` skills.
  This mirrors the embedded copy at
  `cmd/loomcycle/embedded/bundles/agent-teams.yaml` (the one the binary ships);
  keep the two in sync.

## The workflow model (RFC AP)

A `TeamDef`'s definition is a **state-machine graph**: states (nodes) with a
handler each — `agent` | `parallel`+consolidator | `consolidator` | `terminal` —
and transitions (edges) gated by `success` / `pushback:<reason>` /
`conditional:<expr>`. The graph is validated before any write; loops are bounded
by a per-state `max_iterations` cap. Colours are presentation-only (excluded from
the content hash) and drive the generated Mermaid diagram.

**Phase 1** runs single-agent linear teams end to end (`op=run`); `parallel` /
`consolidator` execution + pushback routing render + validate but are not
executable yet — `op=run` returns a clear not-yet-supported error for them.
