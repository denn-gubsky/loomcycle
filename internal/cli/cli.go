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
// `loomcycle help`, `loomcycle -h`, `loomcycle --help`. metaToolCount
// is the live count of MCP meta-tools (mcp.MetaToolCount()), passed in
// from main.go so this package stays free of the mcp dependency and the
// printed count never drifts from the registry.
func PrintHelp(w io.Writer, metaToolCount int) {
	fmt.Fprintln(w, "loomcycle — high-load agentic runtime sidecar")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "USAGE")
	fmt.Fprintln(w, "  loomcycle [--config <yaml>]      start the HTTP+SSE server (default)")
	fmt.Fprintln(w, "  loomcycle <subcommand> [args]    run a one-shot operator subcommand")
	fmt.Fprintln(w, "  loomcycle version | --version    print build identifier (version/commit/built)")
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
	fmt.Fprintln(w, "  presets                          list embedded config presets + bundles (RFC AQ)")
	fmt.Fprintln(w, "  presets show <name>              print one embedded preset/bundle's YAML")
	fmt.Fprintln(w, "  env-template                     print the embedded .env.insecure.example")
	fmt.Fprintln(w, "  health [--target <url>]          GET /healthz against a running instance")
	fmt.Fprintln(w, "  migrate up      [--config <y>]   apply pending Postgres schema migrations")
	fmt.Fprintln(w, "  migrate down    [--config <y>]   roll back Postgres schema migrations")
	fmt.Fprintln(w, "  migrate status  [--config <y>]   show current schema version + dirty flag")
	fmt.Fprintln(w, "  migrate sqlite-to-postgres --src <path> --dst <dsn>")
	fmt.Fprintln(w, "                                   copy SQLite data into Postgres")
	fmt.Fprintln(w, "  mcp [--config <yaml>]            run as MCP server over stdio (v0.8.15+)")
	fmt.Fprintf(w, "                                   exposes %d meta-tools; consumed by Claude\n", metaToolCount)
	fmt.Fprintln(w, "                                   Code, Claude Desktop, custom orchestrators")
	fmt.Fprintln(w, "  mcp install [--transport T]      print Claude Code / Desktop config snippets")
	fmt.Fprintln(w, "              [--config <yaml>]    for registering loomcycle as an MCP server")
	fmt.Fprintln(w, "              [--json]             (T: docker | brew | binary; auto-detected)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "MCP RECIPE LIBRARY (v1.x — RFC C1)")
	fmt.Fprintln(w, "  mcp-registry list                list curated + overlay recipes")
	fmt.Fprintln(w, "               [--format=json] [--include-disabled]")
	fmt.Fprintln(w, "  mcp-registry show <name>         print one recipe's canonical JSON")
	fmt.Fprintln(w, "               [--bundled]         force bundled even if overlay shadows it")
	fmt.Fprintln(w, "  mcp-registry append-to-config <name> --to=<file> [--force] [--skip-env-check]")
	fmt.Fprintln(w, "                                   translate a recipe into mcp_servers: yaml")
	fmt.Fprintln(w, "  mcp-registry add <path> [--name=<n>] [--force]")
	fmt.Fprintln(w, "                                   copy a JSON file into the overlay root")
	fmt.Fprintln(w, "  mcp-registry remove <name>       delete <name>.json from the overlay root")
	fmt.Fprintln(w, "  mcp-registry enable <name>       un-suppress a recipe (clear from .disabled)")
	fmt.Fprintln(w, "  mcp-registry disable <name>      suppress a recipe (add to .disabled)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Overlay root: $LOOMCYCLE_MCP_RECIPES_ROOT (filesystem dir of <name>.json files).")
	fmt.Fprintln(w, "  Without the env var, only bundled recipes are available + the mutation verbs")
	fmt.Fprintln(w, "  (add / remove / enable / disable) refuse with a clear error.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "REPO INGESTION (v1.x — RFC C2)")
	fmt.Fprintln(w, "  import claude-code --from=<path-to-.claude/>     walk a Claude Code repo")
	fmt.Fprintln(w, "                                                   and emit loomcycle yaml")
	fmt.Fprintln(w, "    [--report-only]                                inventory summary only")
	fmt.Fprintln(w, "    [--dry-run --diff=<file>]                      yaml diff against target")
	fmt.Fprintln(w, "    [--write [--force]]                            apply diff to target yaml")
	fmt.Fprintln(w, "    [--no-recipe-match]                            literal-port .mcp.json")
	fmt.Fprintln(w, "                                                   (skip C1 library rewrites)")
	fmt.Fprintln(w, "    [--emit-recipes [--no-yaml]]                   write recipes to overlay")
	fmt.Fprintln(w, "    [--json]                                       render report as JSON")
	fmt.Fprintln(w, "    [--skills-dest=<dir>]                          target for SKILL.md copies")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Walks .claude/agents/, .claude/skills/, .claude/mcp.json (+ <root>/.mcp.json).")
	fmt.Fprintln(w, "  Slash commands in .claude/commands/ are surfaced as SKIPPED (IDE-side UX).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  memory-eval [--dataset bundled|<file.jsonl>]     score the memory ranker/dedup")
	fmt.Fprintln(w, "    [--rank-config <file.json>] [--output <file>]   precision@k / recall@k / dup-rate")
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
