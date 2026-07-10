# agent-teams bundle

Two authoring assistants for RFC AP **Agent Teams & Task Workflows**, plus the
`team/*` skills they use.

| Agent | What it does |
|-------|--------------|
| `agent/assistant` | Helps you design + author a specialized **AgentDef** — the reusable building block a team routes between (role, least-privilege tools, tier, skills, scopes). |
| `team/assistant`  | Assembles a **TeamDef** (a workflow state machine) from agents that *already exist*. It renders the Mermaid diagram, gets your sign-off, commits, and can `run` the team to test it. It does **not** create agents — it points you at `agent/assistant`. |

There is deliberately **no `team/orchestrator` agent**: walking a team's state
graph is the runtime's job (`internal/teamrun`), exposed as **`TeamDef op=run`**.
A deterministic state machine shouldn't be driven by an LLM.

## Select it

```
LOOMCYCLE_PRESETS=base,agent-teams
```

The bundle carries **no provider matrix** — both agents declare `tier: middle`,
so select `base` (or your own config) alongside it to supply a `middle` tier.

## What's inside

- `loomcycle.yaml` — the bundle config: the two agents + the inline `team/structure`
  and `team/workflow` skills. This mirrors the embedded copy at
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
