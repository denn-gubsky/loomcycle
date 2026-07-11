---
name: mcp-registry
description: Curated MCP server recipe library — bundled JSON templates + filesystem overlay + 7-verb CLI.
---

# MCP server recipe library

Loomcycle ships a curated library of MCP server recipes — pre-vetted JSON
templates for the 10-13 most common MCP servers (Tavily, GitHub, Slack,
Telegram, etc.). Operators copy a recipe into their `loomcycle.yaml` via
one CLI command instead of hand-authoring the `mcp_servers:` block.

## Three layers

The library is one of three sources for MCP server names at runtime:

1. **Tier 1 — Recipe library (this).** Read-only template source. Bundled
   recipes ship with the binary; the operator overlay at
   `$LOOMCYCLE_MCP_RECIPES_ROOT` supplements / overrides bundled
   entries (overlay file completely replaces a bundled file of the
   same name). Never auto-loaded into the agent runtime — the library
   is a TEMPLATE source, not a registration source.
2. **Tier 2 — `mcp_servers:` yaml in `loomcycle.yaml`.** The ground
   truth. Boot-loaded into the static MCP registry. `loomcycle
   mcp-registry append-to-config <name>` writes here.
3. **Tier 3 — `MCPServerDef` substrate** (v0.9.2). Runtime CRUD via
   the 8-op tool over HTTP / gRPC / MCP / TS adapter. For dynamic
   per-tenant registration. Yaml (Tier 2) wins on name collision.

## CLI verbs

| Verb | Purpose |
|---|---|
| `mcp-registry list` | Print all enabled recipes with source tags. `--format=json` for machine-readable, `--include-disabled` to surface hidden ones. |
| `mcp-registry show <name>` | Print one recipe's canonical JSON. `--bundled` forces the bundled version even if an overlay shadows it. |
| `mcp-registry append-to-config <name> --to=<file>` | Translate the recipe to YAML, append to the target file's `mcp_servers:` block. Idempotent (refuses to clobber without `--force`). |
| `mcp-registry add <path>` | Copy a JSON file into the overlay root. Refuses without `LOOMCYCLE_MCP_RECIPES_ROOT` set. |
| `mcp-registry remove <name>` | Delete `<name>.json` from the overlay root. Refuses if the name only exists in bundled (use `disable` instead). |
| `mcp-registry enable <name>` | Un-suppress a previously disabled recipe. |
| `mcp-registry disable <name>` | Suppress a recipe (writes to `.disabled` under the overlay root). |

## Recipe file format

Each recipe is one JSON file in Claude Code's `.claude/mcp.json`
per-server shape, plus a sibling `_loomcycle:` metadata block:

```json
{
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-slack"],
  "env": {
    "SLACK_BOT_TOKEN": "${LOOMCYCLE_SLACK_BOT_TOKEN}"
  },

  "_loomcycle": {
    "description": "Slack channel + DM messaging. Schedule-compatible.",
    "transport": "stdio",
    "pool_size": 2,
    "env_vars_required": ["LOOMCYCLE_SLACK_BOT_TOKEN"],
    "credentials": [],
    "schedule_compatible": true,
    "agent_prompt_hint": "Use this to post messages to Slack channels..."
  }
}
```

The top-level fields are byte-compatible with Claude Code's
`.claude/mcp.json` (Claude ignores unknown top-level fields, so the
`_loomcycle:` block is silently passed through). This means:

- A Claude Code repo's `.mcp.json` entry can be lifted into the
  loomcycle overlay by adding the `_loomcycle:` block.
- A loomcycle recipe can be lifted into a `.claude/` repo by
  stripping `_loomcycle:`.
- The `loomcycle import claude-code` walker uses the same format
  on both ends — recipe-match path consults this library, and the
  `--emit-recipes` flag writes lossless `.mcp.json` entries into the
  overlay root.

## Composition with shipped surfaces

- **ScheduleDef** — recipes with `_loomcycle.schedule_compatible: true`
  are valid targets for `on_complete.mcp.call` hooks on scheduled
  runs. The library's `slack`, `telegram`, `email`, `discord`,
  `notion`, `jobs`, and per-server-fork-of `github` / `gitlab`
  recipes ship schedule-compatible.
- **per-run credentials** — recipes that authorize per-user
  (e.g. `jobs`) demonstrate the `${run.credentials.<name>}` header
  substitution pattern. Operator-side env-var pattern
  (`${LOOMCYCLE_FOO_TOKEN}`) is for shared / operator-owned auth.

## What the library is NOT

- Not a runtime registration source. Loomcycle never auto-instantiates
  an MCP server from the library at boot. Operators control
  registration via `mcp_servers:` yaml (Tier 2) or `MCPServerDef`
  runtime tool (Tier 3).
- Not a remote registry. Recipes ship inside the binary
  (`//go:embed`). There is no network fetch, no signing, no
  versioning. Operators wanting custom recipes drop JSON files into
  `$LOOMCYCLE_MCP_RECIPES_ROOT`.
- Not a package installer. `append-to-config` writes yaml. It does
  NOT `npm install`, `docker pull`, or otherwise reach for the
  network. Operators install packages with their own tooling.

For the current list of available recipes on this binary, run:

```
loomcycle mcp-registry list
```

For the canonical JSON of any one recipe:

```
loomcycle mcp-registry show <name>
```
