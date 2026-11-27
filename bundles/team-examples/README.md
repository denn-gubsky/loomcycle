# team-examples bundle

Runnable **starter teams** for the RFC BD `team/orchestrator`. The `agent-teams`
bundle ships the orchestrator + the mechanism; this bundle ships the **content**
so a team runs end to end out of the box.

```
LOOMCYCLE_PRESETS=base,agent-teams,team-examples
```

## What's inside

- **Handler agents** for two domains:
  - software: `sdlc/architect`, `sdlc/coder`, `sdlc/reviewer`
  - marketing: `marketing/writer`, `marketing/editor`
- Two **teams**, under the `teams:` key (RFC GS):
  - **`sdlc`** — a software team (architecture → implementation → review ⇄ implementation → PR) that uses the software workspace (ephemeral `work` volume + `git`/`gh` via Bashbox; see the `agent-teams` `team/repo` skill + its operator prerequisites). `sdlc/coder` and `sdlc/reviewer` build and test in a workspace (RFC GW) when `workspace.mode` is on, the reviewer reads the diff with the `Git` tool, and both keep `Bashbox` as the fallback until RFC GW P15 removes it.
  - **`marketing`** — a Documents-only team (draft → edit ⇄ draft → published) with **no volume/repo/PR** — the Document task board is the deliverable.
- A **`team/examples`** skill: how to drive the two teams and how to fork them.

Two domains on purpose: SDLC exercises the repo workspace; marketing proves the
domain-general design (the workspace is optional — RFC BD).

## The teams are configuration

At boot the server writes `sdlc` and `marketing` into the store's shared tier as
ordinary TeamDef versions marked `source: "config"`. Every tenant can run them by
name (`TeamDef op=run name="sdlc"`), and the handler agents are global so they run
in every tenant too. Ask the orchestrator to *"run the marketing example"* and it
drives the configured team.

To change one, overlay its key in your own config (for example
`teams.sdlc.max_iterations: 8`): an edit of a configured team over the API is
refused. A tenant can `TeamDef op=fork` either team into its own tenant, and its
fork then runs in place of the configured one for that tenant.

## Notes

- The `sdlc/*` handlers are meant to be **spawned by the orchestrator**, which
  creates the shared ephemeral `work` volume; run standalone they have no volume
  and their file ops are denied (expected).
- `loomcycle.yaml` mirrors the embedded copy at
  `internal/embedded/bundles/team-examples.yaml`; keep the two in sync.
- These are starters — fork them (`TeamDef op=fork`) or build your own with the
  `team/assistant` agent.
- A team in a bundle may not carry `local.webhooks`, `hooks`/`tool_hooks` or a
  `definition_file`: a bundle arrives in a deployment unread
  (`internal/embedded/unit_keys_test.go`).
