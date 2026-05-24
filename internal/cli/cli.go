// Package cli implements loomcycle's operator-facing subcommands:
//
//	loomcycle init [--path <dir>] [--interactive]   — create default config (v0.11.1)
//	loomcycle doctor [--config <yaml>]              — health-check setup (v0.11.1)
//	loomcycle validate <yaml>                       — sanity-check a config
//	loomcycle agents list [--config <yaml>]         — describe registered agents
//	loomcycle health [--target <url>]               — ping a running instance
//	loomcycle migrate up|down|status [--config Y]   — run Postgres schema migrations
//	loomcycle migrate sqlite-to-postgres            — copy data between adapters
//	loomcycle mcp [--config <yaml>]                 — run as MCP server (stdio, v0.8.15+)
//
// Each subcommand exposes a Run* function returning an exit code so the
// caller (cmd/loomcycle/main.go) can `os.Exit(rc)` cleanly. Stdout/
// stderr are passed in so tests can assert on the produced output
// without race-driving the global os.Stdout.
//
// Note: `mcp` is special — it's handled directly in main.go (not via
// a Run* function here) because it reuses the full server boot path.
// PrintHelp still lists it for discoverability.
//
// Design intent: these are thin wrappers around existing internal
// packages. No business logic lives here — validate calls config.Load
// and reports its result; migrate calls postgres.MigrateUp; the
// sqlite-to-postgres tool reuses the Store interface for both ends.
// Adding a new subcommand should rarely involve more than ~50 lines
// of glue.
package cli

import (
	"fmt"
	"io"
)

// PrintHelp writes the top-level help text to w. Invoked by
// `loomcycle help`, `loomcycle -h`, `loomcycle --help`.
func PrintHelp(w io.Writer) {
	fmt.Fprintln(w, "loomcycle — high-load agentic runtime sidecar")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  loomcycle [--config <yaml>]      start the HTTP+SSE server (default)")
	fmt.Fprintln(w, "  loomcycle <subcommand> [args]    run a one-shot operator subcommand")
	fmt.Fprintln(w, "  loomcycle --version              print build identifier")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "FIRST-RUN (v0.11.1)")
	fmt.Fprintln(w, "  init [--path <dir>]              write loomcycle.yaml + CONFIGURATION.md")
	fmt.Fprintln(w, "       [--interactive]             (auto-on when TTY); to ~/.config/loomcycle/")
	fmt.Fprintln(w, "       [--no-interactive] [--force]")
	fmt.Fprintln(w, "  doctor [--config <yaml>]         health-check config, env, providers, storage")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "SUBCOMMANDS")
	fmt.Fprintln(w, "  validate <yaml>                  load + validate a config; report issues")
	fmt.Fprintln(w, "  agents list [--config <yaml>]    describe each registered agent")
	fmt.Fprintln(w, "  health [--target <url>]          GET /healthz against a running instance")
	fmt.Fprintln(w, "  migrate up      [--config <y>]   apply pending Postgres schema migrations")
	fmt.Fprintln(w, "  migrate down    [--config <y>]   roll back Postgres schema migrations")
	fmt.Fprintln(w, "  migrate status  [--config <y>]   show current schema version + dirty flag")
	fmt.Fprintln(w, "  migrate sqlite-to-postgres --src <path> --dst <dsn>")
	fmt.Fprintln(w, "                                   copy SQLite data into Postgres")
	fmt.Fprintln(w, "  mcp [--config <yaml>]            run as MCP server over stdio (v0.8.15+)")
	fmt.Fprintln(w, "                                   exposes 20 tools; consumed by Claude Code,")
	fmt.Fprintln(w, "                                   custom MCP orchestrators, etc.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "RUNTIME ADMIN (v0.8.17 — operators of a running instance)")
	fmt.Fprintln(w, "  pause [--timeout-ms N]           POST /v1/_pause — quiesce the runtime")
	fmt.Fprintln(w, "  resume                           POST /v1/_resume — release the pause")
	fmt.Fprintln(w, "  state                            GET  /v1/_state — current state + paused-runs count")
	fmt.Fprintln(w, "  snapshot [--description S]       POST /v1/_snapshots — capture a snapshot")
	fmt.Fprintln(w, "           [--include-history --since RFC3339]")
	fmt.Fprintln(w, "  snapshots list [--limit N]       GET  /v1/_snapshots — list captured snapshots")
	fmt.Fprintln(w, "  snapshots export <id> [--out F]  GET  /v1/_snapshots/<id>/export — write envelope")
	fmt.Fprintln(w, "  snapshots delete <id>            DELETE /v1/_snapshots/<id>")
	fmt.Fprintln(w, "  restore <file.json>              POST /v1/_snapshots/inline/restore — import")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Runtime-admin commands talk to a running server via $LOOMCYCLE_BASE_URL")
	fmt.Fprintln(w, "  (default http://127.0.0.1:8787) + $LOOMCYCLE_AUTH_TOKEN.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run any subcommand with -h for its own flags.")
}

// fail writes "loomcycle: error: <msg>" to stderr and returns 2 — the
// **user-error** exit code. Use for: missing/bad CLI flags, malformed
// config, unknown verbs, references to files that don't exist, etc.
//
// Operational failures (Postgres unreachable, migration engine error,
// network failure on health probe) get exit code 1 via failOp() so
// deployment pipelines can distinguish "fix the invocation" from
// "the runtime/infra is sick."
func fail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "loomcycle: error: "+format+"\n", args...)
	return 2
}

// failOp writes the same prefix to stderr but returns 1 — the
// **operational-failure** exit code. See fail() above for the
// 1-vs-2 split rationale.
func failOp(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "loomcycle: error: "+format+"\n", args...)
	return 1
}
