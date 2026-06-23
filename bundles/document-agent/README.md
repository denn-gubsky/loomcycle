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
1. **Layer this bundle onto your config — no copy-paste** (RFC AN config
   layering): pass both files, your authoritative one **last** so your
   providers/tiers/volumes win while the bundle contributes `doc-manager`:
   ```sh
   loomcycle --config bundles/document-agent/loomcycle.yaml --config your.yaml
   ```
   (Or copy the `agents: doc-manager` block into your config by hand if you'd
   rather keep one file.)
2. Point `LOOMCYCLE_SKILLS_ROOT` at this bundle's `skills/` (or copy them into
   your skills root) so the four skills resolve.
3. Ensure `LOOMCYCLE_SQLMEM_ENABLED=1`.
4. The agent only needs a `middle` tier to resolve — when layered, it uses your
   config's routing (last wins).

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
single **interactive** run of `doc-manager` and steers each instruction in,
prefixed with a `[context]` block: the **first** turn carries the document
**outline** (every chunk's title/type/status/id) plus the **selected chunk's
full content**, so the agent is grounded immediately; later turns carry the live
selection's current content. The agent re-reads with `query_chunks`/`get_chunk`
and edits with the other `Document` ops, all in **`user` scope** — the same scope
the viewer shows (the run's user_id is your principal subject), so its edits
appear when the viewer refreshes.

## Driving documents from an EXTERNAL MCP agent (RFC AG + AO)

The in-process Web UI Assistant above already aligns — the run's `user_id` is your
principal subject, so its edits show in the viewer. If instead you drive the same
documents from a **separate agent over an MCP thin client** (`loomcycle mcp
--upstream`, e.g. a marketing agent in another repo), make that client and the Web
UI authenticate as the **same** identity, or the agent's user-scoped Documents land
under a different user and are invisible in the UI.

Declare one principal and use its token for both (see
`docs/CONFIGURATION.md` "Declared principals"):

```yaml
# loomcycle.yaml
principals:
  marketing: { tenant: acme, subject: marketing, scopes: [runs:create, runs:read, substrate:tenant], token_env: LOOMCYCLE_TOKEN_MARKETING }
```
```sh
# .env.local:  LOOMCYCLE_TOKEN_MARKETING=lct_…
# Web UI:      /ui/login with that token
# MCP client:  LOOMCYCLE_MCP_UPSTREAM_TOKEN=$LOOMCYCLE_TOKEN_MARKETING  → loomcycle mcp --upstream <url>
```

Both resolve to `(acme, marketing)`; the external agent's `Document`/`Memory` writes
(user scope) and the UI's Document tab read the same per-scope store.

## Forward path

The intent is for `doc-manager` to eventually be a **built-in-by-default** agent
— embedded in the binary and auto-registered with no operator config, the way
Claude Code ships built-in agents/skills. That needs a built-in-agent mechanism
loomcycle doesn't have yet; until then, this bundle is the source of truth and
the recommended way to register it.
