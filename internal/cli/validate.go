package cli

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
)

// RunValidate loads a YAML config and reports on its health. Returns:
//
//	0  — config loaded cleanly; agents resolve to (provider, model);
//	     skill bundles satisfy the subset check; storage backend known.
//	2  — config error (missing/malformed file, validate() rejected it,
//	     resolveSkills() failed). Stderr carries a pointed message.
//
// This is the operator's first-line tool when a deploy doesn't start —
// the same error path the runtime takes at startup, but isolated so
// they can iterate on the YAML without bringing the server up.
//
// Reachability checks (MCP server dial, Postgres connection) are NOT
// performed here — those are runtime concerns and would require live
// peers. Validate is a pure parse-and-resolve dry-run.
func RunValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: loomcycle validate <path/to/loomcycle.yaml>")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Loads the YAML, runs every constructor-time check, and prints a")
		fmt.Fprintln(stderr, "summary. Exits 0 on a clean config; 2 on any failure (stderr names")
		fmt.Fprintln(stderr, "the offending agent / yaml line / fix).")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	cfgPath := fs.Arg(0)

	cfg, err := loadLayeredConfig(cfgPath)
	if err != nil {
		return fail(stderr, "config: %v", err)
	}

	// Print the summary table.
	fmt.Fprintf(stdout, "loomcycle config: %s\n", cfgPath)
	fmt.Fprintln(stdout)

	fmt.Fprintf(stdout, "Storage backend  : %s\n", cfg.Storage.Backend)
	if cfg.Storage.Backend == "postgres" {
		dsn := cfg.Storage.PgDSN
		if dsn == "" {
			dsn = "(empty — set storage.pg_dsn or LOOMCYCLE_PG_DSN)"
		}
		fmt.Fprintf(stdout, "Postgres DSN     : %s\n", maskDSN(dsn))
		fmt.Fprintf(stdout, "Auto-migrate     : %v\n", cfg.Storage.PgAutoMigrate)
	}
	fmt.Fprintf(stdout, "Listen address   : %s\n", cfg.Env.ListenAddr)
	fmt.Fprintf(stdout, "Auth token set   : %v\n", cfg.Env.AuthToken != "")
	fmt.Fprintf(stdout, "Concurrency      : max=%d queue=%d timeout=%s\n",
		cfg.Concurrency.MaxConcurrentRuns, cfg.Concurrency.MaxQueueDepth, cfg.Concurrency.QueueTimeout())
	fmt.Fprintln(stdout)

	// Agents. Sort by name for stable output (config map order is
	// non-deterministic across runs).
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(stdout, "Agents           : (none)")
	} else {
		fmt.Fprintf(stdout, "Agents           : %d\n", len(names))
		for _, name := range names {
			provider, model, err := cfg.ResolveAgentModel(name)
			if err != nil {
				return fail(stderr, "agent %q: %v", name, err)
			}
			fmt.Fprintf(stdout, "  %-24s provider=%-10s model=%s\n", name, provider, model)
		}
	}

	if len(cfg.MCPServers) > 0 {
		fmt.Fprintf(stdout, "MCP servers      : %d\n", len(cfg.MCPServers))
		mcpNames := make([]string, 0, len(cfg.MCPServers))
		for name := range cfg.MCPServers {
			mcpNames = append(mcpNames, name)
		}
		sort.Strings(mcpNames)
		for _, name := range mcpNames {
			s := cfg.MCPServers[name]
			fmt.Fprintf(stdout, "  %-24s transport=%s\n", name, s.Transport)
		}
	}

	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "OK")
	return 0
}

// maskDSN replaces the password component of a libpq DSN with "***" so an
// operator running validate in a shared terminal doesn't leak the secret to
// scrollback. Handles BOTH the URL form (postgres://user:pass@host) and the
// keyword form (host=… password=SECRET) — the keyword form was previously
// printed verbatim.
//
// dsnKeywordPassword matches a libpq keyword-form password (`password=SECRET`,
// optionally single-quoted) so maskDSN can redact the value while keeping the
// key. Case-insensitive; captures the value (quoted-with-escapes or unquoted).
var dsnKeywordPassword = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^']|'')*'|\S+)`)

func maskDSN(dsn string) string {
	// libpq keyword form: `host=db user=lc password=SECRET dbname=x`. The
	// URL-form logic below only handles `user:pass@host`, so a keyword DSN
	// (a fully valid LOOMCYCLE_PG_DSN) would print its password verbatim.
	dsn = dsnKeywordPassword.ReplaceAllString(dsn, "${1}***")
	// postgres://user:pass@host/...  →  postgres://user:***@host/...
	at := -1
	for i := 0; i < len(dsn); i++ {
		if dsn[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return dsn
	}
	colon := -1
	// Find the colon between user and pass within the userinfo segment.
	for i := at - 1; i >= 0; i-- {
		if dsn[i] == ':' {
			colon = i
			break
		}
		if dsn[i] == '/' || dsn[i] == '@' {
			break
		}
	}
	if colon < 0 {
		return dsn
	}
	// Don't mask if "user:" with empty password (no chars between colon and @).
	if at-colon < 2 {
		return dsn
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}
