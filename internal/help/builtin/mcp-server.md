---
name: mcp-server
description: Running loomcycle AS an MCP server — embedded vs thin-client (--upstream); the single-runtime invariant; why --no-http is deprecated.
---
loomcycle can act as an **MCP server** so an external orchestrator
(Claude Code, Claude Desktop, a custom client) drives it over standard
MCP — the same meta-tools (`spawn_run`, `cancel_run`, `memory`,
`interruption_resolve`, …) the HTTP `/v1/*` surface exposes. The
entry point is `loomcycle mcp`.

This is the INVERSE of consuming external MCP servers as tools — for
that, see `help(topic="dynamic-mcp")`.

## The single-runtime invariant

**Never run two loomcycle runtimes against the same state.** A runtime
owns providers, the scheduler, the sweepers, and — crucially — an
**in-process event bus** that wakes blocked runs (e.g. an agent parked on
`Interruption.ask`). Two runtimes sharing one `./data` each have their
*own* bus, so a signal raised on one (a resolved interruption, a cancel)
never reaches a run owned by the other. The state row flips, but the
agent never wakes.

So there is exactly one authoritative runtime per state — a single
process, or a backplane-coordinated cluster — and any *additional*
loomcycle process is a **control client**, never a second runtime.

## Two ways to run `loomcycle mcp`

### embedded (standalone / single host)
```
loomcycle mcp --config loomcycle.yaml
```
One process that is BOTH the runtime and the MCP server. Because it's a
single process with a single bus, the cross-process problem can't arise.
Right when the MCP server *is* your loomcycle (a laptop, a dev box).

### thin client (`--upstream`) — when a runtime already exists
```
loomcycle mcp --upstream http://127.0.0.1:8788
# bearer (if the runtime enforces auth): LOOMCYCLE_MCP_UPSTREAM_TOKEN
```
Runs as a **stdio ↔ `/v1/_mcp` proxy** to an already-running runtime and
boots **no runtime of its own** — no providers, scheduler, sweepers,
Store, listener, or bus. Every call (including `interruption_resolve`)
lands on the runtime that owns the run, so it wakes correctly. This is
the supported way to add an MCP surface next to a running server or a
multi-replica cluster (point `--upstream` at any replica / the load
balancer). The proxy returns a clean JSON-RPC error — never hangs — if
the upstream is unreachable or drops a stream.

## `--no-http` is deprecated

`loomcycle mcp --no-http` only *mutes the listener* — it still boots a
**full second runtime** alongside your real one, violating the invariant
above. That two-runtime topology is the root of the cross-process
interruption hang and the "wedged session" failures. **Use `--upstream`
instead.** `--no-http` still works for now (with a deprecation warning)
so the Claude Code plugin keeps running until it migrates; it will be
removed afterward.

## doctor

`loomcycle doctor` WARNs (it doesn't FAIL) when the configured listen
address is already in use — that usually means a runtime already owns
this state. Heed it: don't start a second runtime; add an MCP surface
with `loomcycle mcp --upstream <runtime-url>`.

## Related topics
- `dynamic-mcp` — the inverse: loomcycle consuming external MCP servers.
- `subagents` — the in-loop `Agent` tool vs the MCP `spawn_run` surface.
- `getting-started` — first-run walkthrough.
