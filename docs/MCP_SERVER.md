# Using loomcycle as an MCP server

This page covers the **other direction** of loomcycle's MCP integration: exposing loomcycle itself as a stdio MCP server that **Claude Code, Claude Desktop, and other MCP orchestrators consume**.

> **Why this, not the alternative.** You could write your own orchestration: spin up loomcycle as an HTTP server, write client code that polls `/v1/runs`, manage tokens, parse SSE streams. The MCP server path is one command and a five-line config — every MCP client (Claude Code, Claude Desktop, the model layer of any orchestrator that speaks MCP) discovers loomcycle's meta-tools automatically. The HTTP path stays available for everything else, but for "I want Claude Code to drive a multi-agent loomcycle backend", MCP is the shorter route.

The consumer side (loomcycle's agents calling external MCP servers like `mcp__jobs__getAgentContext`) is documented in [`MCP_INTEGRATION.md`](MCP_INTEGRATION.md). This document is purely about the inverse: loomcycle's own `loomcycle mcp` subcommand.

## What you get

When you register loomcycle as your MCP server, your MCP client gains loomcycle's **meta-tools** for driving it from inside a chat — spawn runs, manage agents, read/write memory, publish/subscribe to channels, register agent definitions, etc. The list your client shows (`tools/list`, or `/mcp` in Claude Code) is the authoritative catalogue: 54 tools at v1.107.0 for an operator, fewer for a token that may not call them all (see *What a token may call* below). The diagram at [`docs/assets/architecture-connector.png`](assets/architecture-connector.png) shows how they dispatch through the Connector.

The most common consumer is Claude Code: you can ask Claude to "spawn a `qa-agent` against this PR and stream the result" and Claude will use loomcycle's `spawn_run` meta-tool transparently.

**v0.33.0 added two run-lifecycle meta-tools:** **`spawn_runs`** — RFC Y external fan-out: spawn up to 32 fresh runs concurrently in one call and get back a combined index-aligned envelope (prefer it over firing N `spawn_run` calls, which serialize over the single stdio connection); and **`compact_run`** — compact a parked run's context by `agent_id` (summarize older turns, continue from the summary). `spawn_run`/`spawn_runs` also now accept per-run `sampling` + `compaction` overrides.

**Added since:** `configured_run` (create a run without starting it, edit it, then start or discard it), `retune_run` (change a running agent's settings without sending it a turn), `review_run` (rule on an answer held for review), `decision` (ask a decision model outside a run), `teamdef` and `hookdef` (author teams and reusable hooks), `credentialdef`, `volumedef` and `documentsourcedef` (stored secrets, filesystem volumes, peer document sources), `path`, `document` and `history` (named paths, chunked documents, past chats), and `directory` and `erasure` (who is in the tenant and what is held for them; report or erase one subject). `register_hook`, `list_hooks` and `delete_hook` were removed in v1.97.0: a run fires the hooks on its agent definition, and reusable ones are authored with `hookdef`.

### Retrying and bounding a run

**Retrying a start safely: `idempotency_key`.** `spawn_run`, each `spawn_runs` child, each `POST /v1/runs:batch` child, `POST /v1/runs` and the gRPC `Run` and `SpawnRunBatch` requests take an optional `idempotency_key` (1 to 200 characters of letters, digits and `: . _ -`). A second request with the same key, from the same tenant and user, starts nothing. It is answered with the run the first request started, with `deduplicated: true`:

- In `detach` mode it returns that run's ids and its current status.
- In `join` mode, and on `spawn_run`, it waits for that run to end and returns its result, read from the run's row. The text is the stored (redacted) answer and `limits` is absent.
- On the streaming surfaces (`POST /v1/runs`, gRPC `Run`) the response is the existing run's stream: the `session` and `agent` frames a fresh run sends, the `agent` frame carrying `deduplicated: true`, then the run's events from its first, ending when the run does. Closing the stream does not stop the run.
- A timeout on such a call ends the wait only. The run was not started by this call, so it is not cancelled, and it is reported as `running`.
- A run that is waiting for a person (parked for input, or held for review) is returned at once as `running`.

The request body is not compared: the same key with a different prompt still returns the first run, so build the key from what makes the work distinct. A key is refused on a continuation (`session_id`), on a configured run, and when two children of one batch share it. It lives as long as the run's session is retained, and travels with a paused run through a snapshot.

**Bounding a run's lifetime: `max_wall_seconds`.** `spawn_run`, each `spawn_runs` child, each `POST /v1/runs:batch` child, `POST /v1/runs` and the gRPC `Run` and `SpawnRunBatch` requests take an optional `max_wall_seconds` (0 or absent = no bound; at most 30 days). It works for any agent, model-driven or code-js.

- Past the limit the run is cancelled, with everything it started, and ends `cancelled` with `stop_reason: "wall_limit"`.
- Waits count, including an interactive run's time parked for input. Time the runtime is paused does not.
- It belongs to the run, not to the call. It is the time bound for a `detach` child, where `timeout_ms` is refused, and it holds after the caller has gone.
- It survives a restart: the limit is in the run's `spec`, and a resumed run is charged the lifetime it had already used.
- A sub-agent takes no limit of its own from it; it ends because its parent's cancel reaches it.
- A run reads its own limit with `Context op=self`: `run_budget.wall_limit_ms` and `wall_elapsed_ms`.

A code-js run's `run_timeout_seconds` is a different bound: active time only, waits excluded. A code-js run that outlives the operator's `LOOMCYCLE_CODE_AGENTS_MAX_WALL_SECONDS` still ends `failed` with `code_agent_wall_limit`; only the per-run `max_wall_seconds` ends a run `cancelled` / `wall_limit`.

A blocking spawn now reports how its run really ended. A run cancelled from outside (`cancel_run`, or its own `max_wall_seconds`) was reported `completed` by `spawn_run` and by a joined batch; it is reported `cancelled`, as its row says.

## Single-runtime invariant: embedded vs thin-client (`--upstream`)

**Never run two loomcycle runtimes against the same state.** A runtime owns the providers, scheduler, sweepers, and an *in-process event bus* that wakes blocked runs (e.g. an agent parked on `Interruption.ask`). Two runtimes sharing one `./data` each have their own bus, so a signal raised on one — a resolved interruption, a cancel — never reaches a run owned by the other: the state row flips, but the agent never wakes. That two-runtime topology is the root of the cross-process interruption hang and the "wedged session" failures.

So there's exactly **one** authoritative runtime per state, and `loomcycle mcp` runs one of two ways:

- **embedded** — `loomcycle mcp --config loomcycle.yaml`. One process that is *both* the runtime and the MCP server. A single process → a single bus → the cross-process problem can't arise. Use this when the MCP server *is* your loomcycle (a laptop, a dev box).
- **thin client** — `loomcycle mcp --upstream http://127.0.0.1:8788` (bearer via `LOOMCYCLE_MCP_UPSTREAM_TOKEN`). Runs as a stdio↔`/v1/_mcp` proxy to an **already-running** runtime and boots **no runtime of its own**. Every call — including `interruption_resolve` — lands on the runtime that owns the run, so it wakes correctly. This is the supported way to add an MCP surface next to a running server or a multi-replica cluster (point `--upstream` at any replica or the load balancer). The proxy returns a clean JSON-RPC error (never hangs) only after self-recovery is exhausted.

> **`--no-http` was removed (v0.23.0).** It only muted the listener while still booting a *full second runtime* — the anti-pattern this whole section replaces. Use `--upstream` (thin client) to add an MCP surface next to a runtime, or plain `loomcycle mcp` for a standalone single-host runtime.

### Connection self-recovery

Claude Code (and MCP hosts generally) **never auto-reconnect a stdio server** — if the proxy surfaces a recoverable fault, you'd have to reload plugins / relaunch. So the proxy owns 100% of recovery and transparently heals the connection so tools keep working across:

- **an idle-reaped keep-alive socket** (the "connection lost a few minutes after the last tool call" symptom — the runtime's HTTP server closes idle connections after 120s). The proxy uses a sub-120s `IdleConnTimeout` so it reopens *before* the server closes, and retries on a fresh connection if one still goes stale.
- **an upstream restart** — a connection refused/reset during the restart window is retried with bounded backoff, and the invalidated session is re-handshaked (replay `initialize` → fresh `Mcp-Session-Id`) then the frame is retried. All single-flighted; the client never sees it.
- **a stalled upstream** — a `ResponseHeaderTimeout` bounds the wait for response *headers* (a streaming `spawn_run` still holds its body open for the whole run) so a hung upstream can't wedge a frame forever.

Tunable via env (sensible defaults; unset = default):

| Env var | Default | Effect |
|---|---|---|
| `LOOMCYCLE_MCP_UPSTREAM_HEADER_TIMEOUT_MS` | `60000` | Max wait for the upstream's response headers before treating the frame as a transport error. |
| `LOOMCYCLE_MCP_UPSTREAM_RECONNECT_ATTEMPTS` | `5` | Number of reconnect attempts after a transport error (immediate, then 500ms → 1s → 2s → 4s backoff). `0` disables retry. |

`loomcycle doctor` WARNs (it doesn't FAIL) when the configured listen address is already in use — that usually means a runtime already owns this state; add an MCP surface with `--upstream`, don't start a second runtime.

## Authentication & tenant confinement (`--upstream`)

The embedded `loomcycle mcp` (stdio) is process-local operator-trust — whoever launched it has full authority, no bearer involved. The thin client is different: it speaks to a running HTTP runtime over `/v1/_mcp`, which is bearer-authed.

- **The bearer is `LOOMCYCLE_MCP_UPSTREAM_TOKEN`** (falls back to `LOOMCYCLE_AUTH_TOKEN`). The proxy forwards it verbatim on every upstream request, so the session authenticates as **that token's principal** — its `(tenant, subject, scopes)` resolved from the token, never the wire.
- **A `substrate:tenant` bearer works (RFC AG).** The `/v1/_mcp` route gate is `substrate:tenant`: a tenant token may open a session, and everything it does is confined to its own tenant — its `agentdef`/`skilldef`/… authoring stamps its tenant, its `memory`/`path`/`document`/`channel` data is keyed under its `(tenant, user)`. User-scoped tools (`document`/`memory`/`path`) key on the bearer's `subject`, so an MCP thin client and the Web UI that authenticate as the *same* principal see the *same* data.
- **Admin-only meta-tools are withheld from a non-admin session.** Token minting (`operatortokendef`), runtime admin (`pause_runtime`/snapshots/…), and cross-scope `list_channels` are runtime-global with no tenant dimension — a `substrate:tenant` session never *sees* them in `tools/list` and is refused (`-32001`) on `tools/call`. A `substrate:admin` bearer sees + may call the full set.
- **Narrower tokens open a session too, and see less.** A tenant member's token reaches `/v1/_mcp` whatever scopes it holds, and an isolated `substrate:user` token is admitted to manage its own credentials. Each tool is then gated on the token's scopes — see *What a token may call* below.

So you can hand a downstream tenant a narrow `substrate:tenant` token and let it drive its own confined MCP session — no admin token required, no cross-tenant reach.

### What a token may call

Since v1.107.0 an MCP session is held to its token's scopes the same way the HTTP API and gRPC are. Which tools a session lists and may call depends on the token:

| Token | Tools |
|---|---|
| No bearer (embedded stdio, or a runtime with no authentication configured) | All of them. |
| `substrate:admin`, including the legacy `LOOMCYCLE_AUTH_TOKEN` | All of them. |
| `substrate:tenant` | The tenant-confinable tools: everything except token minting, runtime pause/resume/state, `resolve_probe`, the snapshot tools and `list_channels`. It satisfies all four scopes in the next table. |
| A member token (no `substrate:admin` or `substrate:tenant`, and not isolated) | The tenant-confinable tools, minus those whose scope in the next table it does not hold. |
| An isolated `substrate:user` token | `credentialdef` only, for its own user-scope credentials. |

These tools need a scope of their own:

| Tool | Scope |
|---|---|
| `spawn_run`, `spawn_runs`, `cancel_run`, `compact_run`, `retune_run`, `review_run`, `configured_run`, `interruption_resolve` | `runs:create` |
| `get_run`, `list_runs`, `stream_user_run_states` | `runs:read` |
| `publish_channel`, `ack_channel` | `channel:publish` |
| `subscribe_channel`, `peek_channel` | `channel:read` |
| `decision` | `runs:create` (it spends tokens) |
| `teamdef` | per operation: `run` and `cancel` need `runs:create`, `poll` needs `runs:read`; the other operations need none |

The remaining tenant-confinable tools have no scope of their own: the definition tools, `register_agent` / `unregister_agent` / `list_agents`, `memory`, `channel`, `path`, `document`, `history`, `evaluation`, `context`, `directory` and `erasure`.

A tool the token may not call is left out of `tools/list`, and a call to it is refused with `-32001`, naming the scope it needs. `teamdef` stays listed for a member; an operation it may not run is refused the same way, naming the operation and the scope. So a tool that is "missing" from a session is usually a scope, not a broken runtime. A token minted with `runs:create` alone can start a run and cannot read it back; mint `runs:create` and `runs:read` together for day-to-day use.

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

Each `add-json` call uses a distinct server-name so both register cleanly. Note this registers instances against **distinct states** (separate `loomcycle.yaml` / data dirs) — that's fine. To point an MCP client at an *already-running* runtime instead of starting another one, use the thin client (`--upstream`); see the single-runtime invariant above.

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
| `mcp__loomcycle__spawn_run` not in tool list | server registered under a different name (`--server-name`); MCP tool names are namespaced by server name. A tool provided through the Claude Code plugin is named `mcp__plugin_loomcycle_loomcycle__spawn_run`. If only some tools are missing, the token may not call them — see *What a token may call* |
| Hangs on first call after a fresh install | loomcycle is downloading provider catalog data on first run; subsequent calls warm |
| Claude Desktop config validates but the server never shows up | restart Claude Desktop fully (not just close the window — Quit from the menu bar) |

## Related docs

- [`MCP_INTEGRATION.md`](MCP_INTEGRATION.md) — consumer side: loomcycle's agents calling external MCP servers
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — request flow, where the MCP server fits in the binary
- [`assets/architecture-connector.png`](assets/architecture-connector.png) — the Connector interface and how MCP/gRPC/CLI consumers dispatch through it
