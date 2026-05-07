package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/denn-gubsky/loomcycle/internal/config"
	storepostgres "github.com/denn-gubsky/loomcycle/internal/store/postgres"
)

// RunMigrate dispatches to one of:
//
//	loomcycle migrate up                  apply pending migrations
//	loomcycle migrate down                roll all migrations back
//	loomcycle migrate status              print version + dirty flag
//	loomcycle migrate sqlite-to-postgres  copy data between adapters
//
// Returns 0 on success, 2 on user error (missing flags, unknown verb,
// config load failure), 1 on operational failure (migration error,
// Postgres unreachable, etc.). The 0/1/2 split lets pipelines
// distinguish "the operator messed up the invocation" from "the
// migration failed" without parsing stderr.
func RunMigrate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: loomcycle migrate {up|down|status|sqlite-to-postgres} [flags]")
		return 2
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "up":
		return runMigrateUp(rest, stdout, stderr)
	case "down":
		return runMigrateDown(rest, stdout, stderr)
	case "status":
		return runMigrateStatus(rest, stdout, stderr)
	case "sqlite-to-postgres":
		return runMigrateSqliteToPostgres(rest, stdout, stderr)
	default:
		return fail(stderr, "unknown migrate verb %q (want up | down | status | sqlite-to-postgres)", verb)
	}
}

// loadPgDSN resolves the operator-visible Postgres DSN: --dsn flag
// wins, then env LOOMCYCLE_PG_DSN, then yaml storage.pg_dsn. The CLI
// migrate verbs accept --config OR --dsn; --dsn is the lower-friction
// path for one-off ops without a yaml in CWD.
func loadPgDSN(cfgPath, explicitDSN string) (string, error) {
	if explicitDSN != "" {
		return explicitDSN, nil
	}
	if cfgPath != "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return "", fmt.Errorf("config: %w", err)
		}
		if cfg.Storage.Backend != "postgres" {
			return "", fmt.Errorf("storage.backend is %q (want \"postgres\") in %s", cfg.Storage.Backend, cfgPath)
		}
		if cfg.Storage.PgDSN == "" {
			return "", fmt.Errorf("storage.pg_dsn is empty in %s and LOOMCYCLE_PG_DSN is unset", cfgPath)
		}
		return cfg.Storage.PgDSN, nil
	}
	if v := getenvDefault("LOOMCYCLE_PG_DSN", ""); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no DSN: pass --dsn, --config <yaml>, or set LOOMCYCLE_PG_DSN")
}

func runMigrateUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config YAML (alternative to --dsn)")
	dsn := fs.String("dsn", "", "Postgres DSN (overrides yaml/env)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedDSN, err := loadPgDSN(*cfgPath, *dsn)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if err := storepostgres.MigrateUp(resolvedDSN); err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: migrate up: %v\n", err)
		return 1
	}
	v, dirty, err := storepostgres.MigrateStatus(resolvedDSN)
	if err != nil {
		// Migration succeeded but post-flight status read failed —
		// not a hard error; just less informative.
		fmt.Fprintln(stdout, "migrate up: OK (status read failed)")
		return 0
	}
	fmt.Fprintf(stdout, "migrate up: OK (version=%d dirty=%v)\n", v, dirty)
	return 0
}

func runMigrateDown(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config YAML (alternative to --dsn)")
	dsn := fs.String("dsn", "", "Postgres DSN (overrides yaml/env)")
	confirm := fs.Bool("yes", false, "confirm the destructive operation (down rolls every migration back)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr, "migrate down rolls every applied migration back — pass --yes to confirm.")
		return 2
	}
	resolvedDSN, err := loadPgDSN(*cfgPath, *dsn)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if err := storepostgres.MigrateDown(resolvedDSN); err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: migrate down: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "migrate down: OK")
	return 0
}

func runMigrateStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config YAML (alternative to --dsn)")
	dsn := fs.String("dsn", "", "Postgres DSN (overrides yaml/env)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedDSN, err := loadPgDSN(*cfgPath, *dsn)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	v, dirty, err := storepostgres.MigrateStatus(resolvedDSN)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: migrate status: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "version: %d\n", v)
	fmt.Fprintf(stdout, "dirty:   %v\n", dirty)
	if v == 0 {
		fmt.Fprintln(stdout, "(no migrations applied — fresh database)")
	}
	return 0
}
