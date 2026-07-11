---
name: claude-code-import
description: How to import a `.claude/` directory into loomcycle yaml — the `loomcycle import claude-code` walker, output modes, recipe-library rewrites, and unmapped-fields catalog.
---

# Claude Code repo ingestion

Loomcycle ships a `.claude/`-shape walker that lifts a Claude Code
repository's agents, skills, and MCP servers into loomcycle yaml.
Operators iterating on agents inside Claude Code can move to
loomcycle without hand-translating four directory trees.

```sh
loomcycle import claude-code --from=./.claude/                # dry-run report
loomcycle import claude-code --from=./.claude/ --report-only  # inventory only
loomcycle import claude-code --from=./.claude/ --dry-run --diff=loomcycle.yaml
loomcycle import claude-code --from=./.claude/ --write
loomcycle import claude-code --from=./.claude/ --write --force
loomcycle import claude-code --from=./.claude/ --emit-recipes --no-yaml
```

## What gets imported

The walker traverses four subdirectories of `.claude/`:

| Source | Becomes | Notes |
|---|---|---|
| `.claude/agents/<name>.md` | `agents.<name>:` block in `loomcycle.yaml` | frontmatter → yaml; body → `system_prompt:` literal block |
| `.claude/skills/<name>/SKILL.md` | `<skills-dest>/<name>/SKILL.md` (filesystem copy) | multi-file skills flagged; supplementary files NOT auto-copied |
| `.claude/mcp.json` AND `<root>/.mcp.json` | `mcp_servers.<name>:` blocks | wrapped (`{"mcpServers": {...}}`) + bare shapes accepted |
| `.claude/commands/<name>.md` | SKIPPED | Claude Code slash commands are IDE-side UX |

`<root>/.mcp.json` (the per-project convention) is read at one
level above the `--from` path.

## v0.12.7 substrate-field heuristics (agents)

When an imported agent's tool list includes `mcp__<server>__*`
entries, the importer emits a `# credentials:` comment listing the
identified servers above the agent's yaml block. The operator
populates `user_credentials:` on `POST /v1/runs` calls (or supplies
them via the schedule-fork `user_credentials` map) when running
those agents in multi-tenant deployments. See
`rfcs/implemented/per-run-credentials.md` for the substitution
pattern.

Two scope stubs land when the agent name matches:

| Name pattern | Stub emitted |
|---|---|
| `*-scheduler`, `*-orchestrator`, `*-scheduling`, `scheduler-*`, `orchestrator-*` | `schedule_def_scopes: ["any"]` + a schedule pointer comment |
| `*-evolver`, `*-author`, `*-meta-*`, `meta-*` | `agent_def_scopes: ["self"]` + safer-floor comment |

The stubs are conservative — `["any"]` and `["self"]` are
operator-tightenable. Default-deny is the safer floor; everything
that doesn't match a pattern gets no stub. Operators tighten or
loosen manually after seeing the agent in action.

## C1 recipe-library rewrite (mcp.json)

For each entry in `.claude/mcp.json`, the importer checks whether
the package matches a recipe in the v1.x curated MCP server recipe
library (`loomcycle mcp-registry list`). When matched, the
recipe's `command` / `args` / `env` / `pool_size` supersede the
operator's literal port — preserving the operator's chosen server
name but applying the canonical recipe shape.

```
REWRITE mcp_servers.my-github → C1 recipe "github" (bundled)
```

The rewrite is opt-OUT (`--no-recipe-match`), not opt-in. Default
behaviour prefers the recipe because:

- C1 recipes use the modern `${LOOMCYCLE_*}` env-var allowlist.
- Bundled recipes use `${run.credentials.<name>}` substitution
  for per-user auth where applicable (shipped).
- `pool_size` defaults are tested.

Operators who've customised their `.claude/mcp.json` (extra args,
custom env shape) and want loomcycle to mirror it byte-for-byte
should pass `--no-recipe-match`.

Overlay recipes at `$LOOMCYCLE_MCP_RECIPES_ROOT` count as match
sources alongside bundled. An operator-authored override in the
overlay wins over the bundled recipe of the same name.

## `--emit-recipes` lossless round-trip

`--emit-recipes` writes each `.claude/mcp.json` entry as a
`<name>.json` file under `$LOOMCYCLE_MCP_RECIPES_ROOT` (in addition
to the `mcp_servers:` yaml emission, or — with `--no-yaml` — instead
of). The format is byte-compatible with C1's recipe library, so
overlay files can be lifted back into a `.claude/` repo by stripping
the `_loomcycle:` metadata block.

Useful when:

- The operator wants the imported entries to be reusable across
  multiple loomcycle deployments. The recipe library is portable;
  `loomcycle.yaml` is per-deployment.
- The `.claude/mcp.json` already contains a customised version of
  a server and the operator wants it as the canonical recipe.

The `_loomcycle:` metadata block is filled with sensible defaults
(`description` derived from name + transport, `pool_size: 2`,
`schedule_compatible: false`); operators edit the file post-import.

`--emit-recipes` REFUSES with a clear error when
`LOOMCYCLE_MCP_RECIPES_ROOT` is unset. No silent skips.

## Output modes

| Flag | Behaviour |
|---|---|
| (default) | Dry-run report to stdout. Nothing written. |
| `--report-only` | Inventory summary only. One line. |
| `--dry-run --diff=<file>` | Show the yaml fragments that would be appended to `<file>`. |
| `--write` | Apply the diff. Refuses to clobber existing entries. |
| `--write --force` | Allow clobber. The report still names every overwrite. |
| `--json` | Render the report as indented JSON. |
| `--no-recipe-match` | Disable the C1 recipe-library rewrite layer. |
| `--emit-recipes` | Write `.mcp.json` entries to `$LOOMCYCLE_MCP_RECIPES_ROOT`. |
| `--no-yaml` | Pair with `--emit-recipes` — skip `mcp_servers:` yaml emission. |
| `--skills-dest=<dir>` | Destination for SKILL.md copies. Default: `<cwd>/skills`. |

## Unmapped fields catalog

The walker is loud about lossy import. These fields are recognised
but deliberately not translated:

**Agent frontmatter (`.claude/agents/<name>.md`):**
- `hooks` — Claude Code-side hooks. No loomcycle equivalent.
- `output_style` / `output-style` — Claude Code-side UX (e.g.
  `/learning`). Not part of the agent runtime.
- `temperature` / `top_p` — Provider-side sampling controls.
  Loomcycle exposes via tier policy, not per-agent.
- `subagents` — Claude Code subagent declarations. Loomcycle's
  Agent-tool spawn pattern is fundamentally different. See
  `rfcs/implemented/agent-tool-fan-out.md`.
- `color` — IDE-side UX. Dropped.
- Any custom key — dropped with a generic "no loomcycle equivalent"
  hint. Run `loomcycle validate` after `--write` to confirm yaml
  validity.

**`.claude/mcp.json`:**
- `registries:` — Loomcycle has no remote-registry surface (sharp
  edge: airgapped-friendly). If a server from a registry is
  in scope, add it manually via `mcp_servers:` or register at
  runtime via the `MCPServerDef` tool.

## Safety semantics

- **Dry-run first.** The default behaviour is dry-run. Operators
  see exactly what the import would do before passing `--write`.
- **No clobber without `--force`.** Re-running on the same target
  refuses to overwrite existing `agents.<name>:` or
  `mcp_servers.<name>:` entries. Skill files refuse to overwrite
  the SKILL.md destination.
- **Lossy import is loud.** Every unmapped field, every skipped
  file, every recipe-library rewrite appears in the report.
- **No reverse export.** Loomcycle does NOT generate `.claude/`-
  shaped output. IDE-as-truth for authoring; loomcycle consumes.
- **Schema validation is separate.** Run `loomcycle validate
  <target.yaml>` after `--write`. The importer produces well-
  shaped yaml; the validator owns "is this yaml internally
  consistent?"

## Worked example

Source `.claude/` tree:

```
.claude/
├── agents/
│   ├── coder.md                 (model + tools, no mcp__)
│   └── jobs-scheduler.md        (mcp__jobs__ tools, name matches scheduler)
├── skills/
│   └── yaml-fence/
│       └── SKILL.md
├── mcp.json                     (github + jobs servers)
└── commands/
    └── snapshot.md              (SKIPPED)
```

Run:

```sh
loomcycle import claude-code --from=./.claude/ --write \
  --diff=./loomcycle.yaml \
  --skills-dest=./skills
```

Produces:

```yaml
agents:
  coder:
    model: claude-sonnet-4-6
    tools: [Read, Write, Edit]
    system_prompt: |
      ...
  # description: Daily scheduler for job pipelines
  # credentials: jobs
  jobs-scheduler:
    model: claude-sonnet-4-6
    tools: [Read, mcp__jobs__getAgentContext]
    schedule_def_scopes: ["any"]
    system_prompt: |
      ...

mcp_servers:
  github:
    transport: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-github"]
    env: {GITHUB_PERSONAL_ACCESS_TOKEN: "${LOOMCYCLE_GITHUB_TOKEN}"}
    pool_size: 4  # from matched C1 recipe
  jobs:
    transport: stdio
    command: npx
    args: ["-y", "@scope/server-jobs"]
    pool_size: 2
```

Plus filesystem copies:

```
./skills/yaml-fence/SKILL.md   ← copied from .claude/skills/yaml-fence/SKILL.md
```

Plus a report on stdout:

```
applying to ./loomcycle.yaml
  copied .../yaml-fence/SKILL.md → ./skills/yaml-fence/SKILL.md
  wrote mcp_servers.github into ./loomcycle.yaml
  wrote mcp_servers.jobs into ./loomcycle.yaml
  wrote agents.coder into ./loomcycle.yaml
  wrote agents.jobs-scheduler into ./loomcycle.yaml

would import 2 agents, 1 skills, 2 mcp servers; 1 files skipped, 0 unmapped fields, 1 warnings
Done. Run `loomcycle validate ./loomcycle.yaml` to confirm.
```

## Troubleshooting

- **"--emit-recipes requires LOOMCYCLE_MCP_RECIPES_ROOT"**: set the
  env var before re-running. The flag refuses loudly rather than
  silently skipping.
- **"--no-yaml is only valid with --emit-recipes"**: `--no-yaml`
  exists only to suppress the `mcp_servers:` yaml emission when
  the operator wants the overlay-only path.
- **"mcp_servers.X already exists"**: pass `--force` to allow
  clobber. The original entries are NOT backed up — use git to
  recover if needed.
- **Env-var allowlist gaps**: the report flags `${FOO} →
  ${LOOMCYCLE_FOO} (NOT in env allowlist — add it manually)` for
  rewrites where the new name isn't in the operator's allowlist.
  Add the names to your env allowlist before running the agent.
- **Multi-file skills**: supplementary files (anything besides
  SKILL.md) appear in the report's warnings and the `SkillEntry.
  SupplementaryAny` list. They are NOT auto-copied. Approach A
  bundling in loomcycle reads only SKILL.md; if your skill needs
  supplementary content, inline it into SKILL.md or restructure.
- **`registries:` in `.claude/mcp.json`**: appears in unmapped
  fields with a fixed message pointing at `MCPServerDef` (sharp
  edge: no remote-registry surface).
