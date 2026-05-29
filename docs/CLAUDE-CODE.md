# Using loomcycle from Claude Code

There are two ways to drive loomcycle from Claude Code. Pick based on how much
UX you want.

> **Why this, not the alternative.** You can register loomcycle's MCP server
> manually with `loomcycle mcp install` and call its 30+ meta-tools by hand —
> that works, but you compose each `op` discriminator and remember each input
> schema yourself. The **plugin** pre-wires the server and adds slash commands
> + skills, so common workflows are one command instead of a hand-authored
> tool call. The manual path stays fully supported for operators who prefer it.

## Recommended: the Claude Code plugin

[`claude-code-plugin-loomcycle`](https://github.com/denn-gubsky/claude-code-plugin-loomcycle)
is a Claude Code plugin that pre-wires the `loomcycle mcp` server and adds:

- **6 slash commands** — `/loomcycle:run`, `/loomcycle:runs`,
  `/loomcycle:cancel`, `/loomcycle:snapshot`, `/loomcycle:eval`,
  `/loomcycle:connect`.
- **4 skills** — spawn-evaluator, replay-failed-run, diff-agentdefs,
  import-claude-code.
- **2 opt-in hooks** — run telemetry + auto-snapshot-on-error (both disabled by
  default).

Install:

```text
/plugin marketplace add denn-gubsky/claude-code-plugin-loomcycle
/plugin install loomcycle
```

You'll be prompted for the loomcycle binary path, your `loomcycle.yaml` path,
the `LOOMCYCLE_AUTH_TOKEN` bearer (stored in your OS keychain), and the base
URL. The plugin then launches `loomcycle mcp --config <your.yaml>` automatically.

The plugin **does not** bundle or start loomcycle — install loomcycle separately
(Homebrew / Docker / release binary) and have an instance reachable first.

## Manual: `loomcycle mcp install`

If you'd rather not use the plugin, register the MCP server directly:

```sh
loomcycle mcp install            # prints a `claude mcp add` line + JSON snippet
```

Paste the snippet into Claude Code's config. Claude Code then sees loomcycle's
meta-tools (`spawn_run`, `cancel_run`, `list_runs`, snapshot ops, `evaluation`,
`agentdef`, …) as ordinary MCP tools. See [`MCP_SERVER.md`](MCP_SERVER.md) for
the full meta-tool reference and transport options (docker / brew / binary).

## Which should I use?

| | Plugin | `mcp install` |
|---|---|---|
| Setup | `/plugin install` once | paste a config snippet |
| UX | slash commands + skills | raw MCP tools |
| Best for | day-to-day operation from the IDE | scripting / custom orchestration |

Both consume the same `loomcycle mcp` server — the plugin is the UX layer on top.
