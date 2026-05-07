// Package cli implements loomcycle's operator-facing subcommands:
//
//	loomcycle validate <yaml>                       — sanity-check a config
//	loomcycle agents list [--config <yaml>]         — describe registered agents
//	loomcycle health [--target <url>]               — ping a running instance
//	loomcycle migrate up|down|status [--config Y]   — run Postgres schema migrations
//	loomcycle migrate sqlite-to-postgres            — copy data between adapters
//
// Each subcommand exposes a Run* function returning an exit code so the
// caller (cmd/loomcycle/main.go) can `os.Exit(rc)` cleanly. Stdout/
// stderr are passed in so tests can assert on the produced output
// without race-driving the global os.Stdout.
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
	fmt.Fprintln(w, "SUBCOMMANDS")
	fmt.Fprintln(w, "  validate <yaml>                  load + validate a config; report issues")
	fmt.Fprintln(w, "  agents list [--config <yaml>]    describe each registered agent")
	fmt.Fprintln(w, "  health [--target <url>]          GET /healthz against a running instance")
	fmt.Fprintln(w, "  migrate up      [--config <y>]   apply pending Postgres schema migrations")
	fmt.Fprintln(w, "  migrate down    [--config <y>]   roll back Postgres schema migrations")
	fmt.Fprintln(w, "  migrate status  [--config <y>]   show current schema version + dirty flag")
	fmt.Fprintln(w, "  migrate sqlite-to-postgres --src <path> --dst <dsn>")
	fmt.Fprintln(w, "                                   copy SQLite data into Postgres")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run any subcommand with -h for its own flags.")
}

// fail writes "loomcycle: error: <msg>" to stderr and returns the
// supplied exit code. Convenience for the early-exit branches.
func fail(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "loomcycle: error: "+format+"\n", args...)
	return 2
}
