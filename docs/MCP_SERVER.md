# Using loomcycle as an MCP server

This page covers the **other direction** of loomcycle's MCP integration: exposing loomcycle itself as a stdio MCP server that **Claude Code, Claude Desktop, and other MCP orchestrators consume**.

> **Why this, not the alternative.** You could write your own orchestration: spin up loomcycle as an HTTP server, write client code that polls `/v1/runs`, manage tokens, parse SSE streams. The MCP server path is one command and a five-line config — every MCP client (Claude Code, Claude Desktop, the model layer of any orchestrator that speaks MCP) discovers loomcycle's 39 meta-tools automatically. The HTTP path stays available for everything else, but for "I want Claude Code to drive a multi-agent loomcycle backend", MCP is the shorter route.

The consumer side (loomcycle's agents calling external MCP servers like `mcp__jobs__getAgentContext`) is documented in [`MCP_INTEGRATION.md`](MCP_INTEGRATION.md). This document is purely about the inverse: loomcycle's own `loomcycle mcp` subcommand.

## What you get

When you register loomcycle as your MCP server, your MCP client gains **39 meta-tools** for driving loomcycle from inside a chat — spawn runs, manage agents, read/write memory, publish/subscribe to channels, register agent definitions, etc. The full surface is enumerated in the diagram at [`docs/assets/architecture-connector.png`](assets/architecture-connector.png).

The most common consumer is Claude Code: you can ask Claude to "spawn a `qa-agent` against this PR and stream the result" and Claude will use loomcycle's `spawn_run` meta-tool transparently.

## Quickest path — `loomcycle mcp install`

`loomcycle mcp install` prints copy-paste-ready snippets for both Claude Code (CLI) and Claude Desktop (Mac app). It auto-detects whether you have Docker, Homebrew, or a direct binary installed and chooses the lowest-friction transport:

```sh
loomcycle mcp install
```

This prints:

```
Transport: docker
Config:    ~/.config/loomcycle/loomcycle.yaml

── Claude Code (CLI) ───────────────────────────────────────
Register the server (one command):

  claude mcp add-json loomcycle '{"command":"docker","args":["run","--rm","-i", ...]}'

Verify it loaded:

  claude mcp list

── Claude Desktop ──────────────────────────────────────────
Edit your claude_desktop_config.json:

  ~/Library/Application Support/Claude/claude_desktop_config.json

Paste this entry under the top-level "mcpServers" object:

  "mcpServers": {
    "loomcycle": { ... }
  }
```

Flags:

| Flag | Default | Purpose |
|---|---|---|
| `--transport docker\|brew\|binary` | auto-detect | Override the auto-detected transport |
| `--config <path>` | `~/.config/loomcycle/loomcycle.yaml` | Which loomcycle.yaml the MCP server should use |
| `--server-name <name>` | `loomcycle` | Name under which the server registers (useful for running multiple instances, e.g. `loomcycle-prod` vs `loomcycle-staging`) |
| `--docker-image <image>` | `denngubsky/loomcycle:latest` | Pin to a specific image tag |
| `--json` | off | Print just the JSON snippet — useful for piping into `jq` or scripts |

`loomcycle mcp install` never writes to your Claude config file — it prints what you should paste/run, you decide where it goes. Auto-merging into someone else's JSON config is a foot-gun.

## Manual setup

If you'd rather copy-paste from this doc instead of running the install command, pick your transport below.

### Option 1: Docker (recommended for Claude Desktop users)

The Docker image (`denngubsky/loomcycle`, multi-arch since v0.11.2) handles env-var passthrough cleanly via `-e` flags. This is the lowest-friction path if you already have Docker installed.

**Claude Code (CLI):**

```sh
claude mcp add-json loomcycle '{
  "command": "docker",
  "args": [
    "run", "--rm", "-i",
    "-v", "'"$HOME"'/.config/loomcycle:/etc/loomcycle:ro",
    "-e", "ANTHROPIC_API_KEY",
    "-e", "OPENAI_API_KEY",
    "-e", "DEEPSEEK_API_KEY",
    "-e", "GEMINI_API_KEY",
    "-e", "LOOMCYCLE_AUTH_TOKEN",
    "denngubsky/loomcycle:latest",
    "mcp", "--config", "/etc/loomcycle/loomcycle.yaml"
  ]
}'
```

**Claude Desktop:** open `claude_desktop_config.json` (path below) and add this under `"mcpServers"`:

```json
{
  "mcpServers": {
    "loomcycle": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "-v", "/Users/YOU/.config/loomcycle:/etc/loomcycle:ro",
        "-e", "ANTHROPIC_API_KEY",
        "-e", "LOOMCYCLE_AUTH_TOKEN",
        "denngubsky/loomcycle:latest",
        "mcp", "--config", "/etc/loomcycle/loomcycle.yaml"
      ],
      "env": {
        "ANTHROPIC_API_KEY": "sk-ant-...",
        "LOOMCYCLE_AUTH_TOKEN": "..."
      }
    }
  }
}
```

> Claude Desktop spawns MCP servers with a sparse environment, so you must populate the `env` object with concrete values (or use the Docker `-e` passthrough by exporting them globally on your system). Claude Code (CLI) inherits the parent shell's env, so plain `-e KEY` (no value) works for CLI users.

### Option 2: Homebrew-installed binary

If you've installed loomcycle via `brew install denn-gubsky/loomcycle/loomcycle`, the binary is on your `$PATH`:

```sh
claude mcp add-json loomcycle '{
  "command": "loomcycle",
  "args": ["mcp", "--config", "'"$HOME"'/.config/loomcycle/loomcycle.yaml"]
}'
```

Same env caveat as above for Claude Desktop — populate `"env"` with concrete values, OR wrap with a shell script that sources your `.env` before exec-ing the binary (see [`loomcycle-mcp.sh`](../loomcycle-mcp.sh) in the repo for a reference wrapper).

### Option 3: Direct binary

If you've downloaded a release tarball or built from source:

```sh
claude mcp add-json loomcycle '{
  "command": "/absolute/path/to/loomcycle",
  "args": ["mcp", "--config", "/absolute/path/to/loomcycle.yaml"]
}'
```

## Config file locations

Claude Desktop's `claude_desktop_config.json` location is platform-specific:

| Platform | Path |
|---|---|
| macOS | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| Linux | `~/.config/Claude/claude_desktop_config.json` |
| Windows | `%APPDATA%\Claude\claude_desktop_config.json` |

Claude Code (CLI) stores MCP server registrations differently — use `claude mcp list` to see what's registered, `claude mcp remove <name>` to unregister.

## Verify

After registering:

```sh
# Claude Code
claude mcp list           # should show 'loomcycle' (and any others)

# Or check that loomcycle's meta-tools are visible inside a Claude session:
claude                    # start a chat
> /mcp                    # lists connected MCP servers + tool counts
> use the spawn_run tool from loomcycle to ...
```

Claude Desktop: restart the app after editing the JSON. Open the Settings → Developer panel; loomcycle should appear with a green dot.

## Running multiple instances side-by-side

`--server-name` lets you register multiple loomcycle instances against the same MCP client. Common case: one for staging, one for production, each pointing at a different `loomcycle.yaml`:

```sh
loomcycle mcp install --server-name loomcycle-staging \
  --config ~/.config/loomcycle/staging.yaml | tee /tmp/staging.snippet

loomcycle mcp install --server-name loomcycle-prod \
  --config ~/.config/loomcycle/prod.yaml | tee /tmp/prod.snippet
```

Each `add-json` call uses a distinct server-name so both register cleanly.

## What if loomcycle.yaml uses ${...} env-var placeholders?

MCP clients spawn servers with **sparse environments by design** — they don't inherit your interactive shell. If your `loomcycle.yaml` has lines like:

```yaml
providers:
  anthropic:
    api_key: ${ANTHROPIC_API_KEY}
```

…the `${ANTHROPIC_API_KEY}` expansion will fail because no `ANTHROPIC_API_KEY` is set in the spawned process.

**Three ways to fix this:**

1. **Docker `-e` flags** (recommended). The Docker transport propagates each named env var from the parent shell, no per-client env config needed (Claude Code only — Claude Desktop still needs concrete values in `env`).
2. **Concrete values in the MCP client's `env`.** Edit your `claude_desktop_config.json` and paste the actual API key into the `env` object. Token is then stored in plaintext in the config file — set the file's mode to `chmod 600` and treat it like an SSH private key.
3. **Wrap with a shell script.** See `loomcycle-mcp.sh` in the repo root: it sources `.env.local`, then exec's `loomcycle mcp`. Register the wrapper's path as the `command` instead of the bare binary.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `claude mcp list` shows loomcycle as red / not connected | spawn failed — try running the command from your terminal manually; check stderr for missing env vars or config-file path issues |
| Tools listed but every call returns "no provider configured" | API key env vars not reaching the MCP server process; use Docker `-e` or populate the client's `env` block |
| `mcp__loomcycle__spawn_run` not in tool list | server registered under a different name (`--server-name`); MCP tool names are namespaced by server name |
| Hangs on first call after a fresh install | loomcycle is downloading provider catalog data on first run; subsequent calls warm |
| Claude Desktop config validates but the server never shows up | restart Claude Desktop fully (not just close the window — Quit from the menu bar) |

## Related docs

- [`MCP_INTEGRATION.md`](MCP_INTEGRATION.md) — consumer side: loomcycle's agents calling external MCP servers
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — request flow, where the MCP server fits in the binary
- [`assets/architecture-connector.png`](assets/architecture-connector.png) — the 36-method Connector interface and how MCP/gRPC/CLI consumers dispatch through it
