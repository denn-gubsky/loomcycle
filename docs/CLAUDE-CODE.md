# Using loomcycle from Claude Code

There are two ways to drive loomcycle from Claude Code. Pick based on how much
UX you want.

> **Why this, not the alternative.** You can register loomcycle's MCP server
> manually with `loomcycle mcp install` and call its meta-tools by hand —
> that works, but you compose each `op` discriminator and remember each input
> schema yourself. The **plugin** pre-wires the server and adds slash commands
> + skills, so common workflows are one command instead of a hand-authored
> tool call. The manual path stays fully supported for operators who prefer it.

## Recommended: the Claude Code plugin

[`claude-code-plugin-loomcycle`](https://github.com/denn-gubsky/claude-code-plugin-loomcycle)
is a Claude Code plugin that pre-wires the `loomcycle mcp` server, as a thin
client of a loomcycle runtime you already run, and adds:

- **Slash commands** — `/loomcycle:connect`, `/loomcycle:run`,
  `/loomcycle:fanout`, `/loomcycle:runs`, `/loomcycle:cancel`,
  `/loomcycle:retune`, `/loomcycle:steer`, `/loomcycle:review`,
  `/loomcycle:compact`, `/loomcycle:decide`, `/loomcycle:eval`,
  `/loomcycle:memory`, `/loomcycle:erasure`, `/loomcycle:snapshot`,
  `/loomcycle:operator-token`. The last two need an admin token.
- **Skills** — `loomcycle-spawn-evaluator`, `loomcycle-replay-failed-run`,
  `loomcycle-diff-agentdefs`, `loomcycle-import-claude-code`,
  `loomcycle-memory`, `loomcycle-configure`.
- **Opt-in hooks** — capture-run-telemetry and auto-snapshot-on-error (both
  disabled by default). These are Claude Code hooks, which watch the IDE's
  tool calls; they are unrelated to loomcycle's own agent hooks.

Install:

```text
/plugin marketplace add denn-gubsky/claude-code-plugin-loomcycle
/plugin install loomcycle@loomcycle
```

You'll be prompted for the loomcycle binary path, the base URL of the running
runtime, and a bearer token for it (stored in your OS keychain). The plugin
then launches `loomcycle mcp --upstream <base_url>` automatically, sending the
token as `LOOMCYCLE_MCP_UPSTREAM_TOKEN`.

The plugin **does not** bundle or start loomcycle, and it loads no
`loomcycle.yaml` of its own — install loomcycle separately (Homebrew / Docker /
release binary) and have a runtime running at that URL first.

What the token may do decides which tools the session sees: an admin token
sees everything, a `substrate:tenant` token everything inside its tenant, and
a narrower token needs `runs:create` to start runs and `runs:read` to read
them. With the plugin's server, Claude Code names the tools
`mcp__plugin_loomcycle_loomcycle__<tool>`; a server you register yourself
under the name `loomcycle` gets `mcp__loomcycle__<tool>`.

> **Single-runtime invariant (RFC R).** The plugin's MCP server is a **thin
> client** of your runtime, not a second runtime:
> `loomcycle mcp --upstream <runtime-url>` proxies to the running runtime and
> boots no runtime of its own — which is also what makes `interruption_resolve`
> and `cancel_run` work correctly across the connection. Early plugin versions
> launched the embedded form (`--config`); current ones do not. See
> [`MCP_SERVER.md`](MCP_SERVER.md) and `Context.help mcp-server`.

## Manual: `loomcycle mcp install`

If you'd rather not use the plugin, register the MCP server directly:

```sh
loomcycle mcp install            # prints a `claude mcp add` line + JSON snippet
```

Paste the snippet into Claude Code's config. Claude Code then sees loomcycle's
meta-tools (`spawn_run`, `cancel_run`, `list_runs`, snapshot ops, `evaluation`,
`agentdef`, …) as ordinary MCP tools. See [`MCP_SERVER.md`](MCP_SERVER.md) for
the full meta-tool reference and transport options (docker / brew / binary).

The snippet starts an embedded runtime (`loomcycle mcp --config …`). If a
loomcycle server is already running against the same state, register the thin
client instead (`loomcycle mcp --upstream <runtime-url>`), as the plugin does —
see the single-runtime invariant in [`MCP_SERVER.md`](MCP_SERVER.md).

## Which should I use?

| | Plugin | `mcp install` |
|---|---|---|
| Setup | `/plugin install` once | paste a config snippet |
| UX | slash commands + skills | raw MCP tools |
| Best for | day-to-day operation from the IDE | scripting / custom orchestration |

Both consume the same `loomcycle mcp` server — the plugin is the UX layer on top.
