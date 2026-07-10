# team-examples bundle

Runnable **starter teams** for the RFC BD `team/orchestrator`. The `agent-teams`
bundle ships the orchestrator + the mechanism; this bundle ships the **content**
so a team runs end to end out of the box.

```
LOOMCYCLE_PRESETS=base,agent-teams,team-examples
```

## What's inside

- **Handler agents** for two domains (agents *can* be static config; TeamDefs cannot):
  - software: `sdlc/architect`, `sdlc/coder`, `sdlc/reviewer`
  - marketing: `marketing/writer`, `marketing/editor`
- A **`team/examples`** skill carrying the ready-to-create TeamDef JSON for:
  - **`sdlc`** — a software team (architecture → implementation → review ⇄ implementation → PR) that uses the software workspace (ephemeral `work` volume + `git`/`gh` via Bashbox; see the `agent-teams` `team/repo` skill + its operator prerequisites).
  - **`marketing`** — a Documents-only team (draft → edit ⇄ draft → published) with **no volume/repo/PR** — the Document task board is the deliverable.

Two domains on purpose: SDLC exercises the repo workspace; marketing proves the
domain-general design (the workspace is optional — RFC BD).

## Why the teams are in a skill, not config

TeamDefs are **runtime-only** (RFC AP has no static team layer — they're created
via `TeamDef op=create`). So a starter team ships as JSON the orchestrator/assistant
instantiates on request, not as YAML. Ask the orchestrator to *"set up and run the
marketing example"* and it creates the TeamDef from `team/examples` and drives it.

## Notes

- The `sdlc/*` handlers are meant to be **spawned by the orchestrator**, which
  creates the shared ephemeral `work` volume; run standalone they have no volume
  and their file ops are denied (expected).
- `loomcycle.yaml` mirrors the embedded copy at
  `cmd/loomcycle/embedded/bundles/team-examples.yaml`; keep the two in sync.
- These are starters — fork them (`TeamDef op=fork`) or build your own with the
  `team/assistant` agent.
