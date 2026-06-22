# document-agent bundle

The **`doc-manager`** agent + document-management skills that power the loomcycle
Web UI's **Document Assistant** (RFC AM Phase 3). Instead of a complex manual
chunk-editing UI, the operator types plain-text instructions and this agent
performs the structural work on a chunked-graph Document (RFC AK): semantic
import, moving chunks, linking edges, deleting, Markdown round-trip.

This is a **first-class, reusable bundle** — not an `examples/` experiment.

```
bundles/document-agent/
├── loomcycle.yaml                 # the doc-manager agent (single-provider)
├── loomcycle_oauth.yaml           # OAuth-FIRST variant (subscription → fallbacks)
├── skills/
│   ├── semantic-chunking/SKILL.md # split prose into a chunk hierarchy
│   ├── edge-linking/SKILL.md      # create/curate graph edges
│   ├── restructuring/SKILL.md     # move/reorder/promote/delete chunks
│   └── md-import/SKILL.md         # import_md (round-trip) vs semantic import
├── examples/
│   └── sample-plan.md             # a plain .md to try the Assistant on
└── README.md
```

## Enable it

The agent must be present in the **running** config for the Web UI Assistant to
find it (loomcycle has no built-in-agent-in-binary mechanism yet — see *Forward
path* below). Two ways:

**Run the bundle directly (demo):**
```sh
export LOOMCYCLE_SQLMEM_ENABLED=1                       # Documents require SQL Memory
export LOOMCYCLE_SKILLS_ROOT=bundles/document-agent/skills
loomcycle --config bundles/document-agent/loomcycle.yaml
```

**Register in your own deployment:**
1. Copy the `agents: doc-manager` block from `loomcycle.yaml` into your config.
2. Point `LOOMCYCLE_SKILLS_ROOT` at this bundle's `skills/` (or copy them into
   your skills root) so the four skills resolve.
3. Ensure `LOOMCYCLE_SQLMEM_ENABLED=1`.
4. Swap the provider/tier for your routing — the agent only needs a `middle`
   tier to resolve.

**Anthropic OAuth (subscription) variant:** `loomcycle_oauth.yaml` is the same
agent with `anthropic-oauth-dev` first in every tier and the API-key/cloud
providers (deepseek/gemini/anthropic/openai) as the fallback cascade. Opt into
the OAuth path (`LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1` + `loomcycle anthropic
login`; see `docs/PROVIDERS.md`) and run it instead:
```sh
export LOOMCYCLE_SQLMEM_ENABLED=1 LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1
export LOOMCYCLE_SKILLS_ROOT=bundles/document-agent/skills
loomcycle anthropic login
loomcycle --config bundles/document-agent/loomcycle_oauth.yaml
```

The Web UI Assistant targets the agent named **`doc-manager`** by default; if it
isn't registered, the Assistant panel shows a hint pointing here instead of a
broken spawn.

## How the Assistant drives it

When you open a Document in the Web UI and use the Assistant, the panel spawns a
single **interactive** run of `doc-manager` with `metadata: {document_id, scope}`
and steers each instruction in, prefixed with a machine line
`[ctx] selected_chunk_id=<id>` so the agent always knows your live selection. The
agent reads with `query_chunks`/`get_chunk` and edits with the other `Document`
ops, all in **`user` scope** — the same scope the viewer shows (the run's
user_id is your principal subject), so its edits appear when the viewer refreshes.

## Forward path

The intent is for `doc-manager` to eventually be a **built-in-by-default** agent
— embedded in the binary and auto-registered with no operator config, the way
Claude Code ships built-in agents/skills. That needs a built-in-agent mechanism
loomcycle doesn't have yet; until then, this bundle is the source of truth and
the recommended way to register it.
