package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

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

// loadPgDSN resolves the Postgres DSN a migrate verb acts on: the --dsn flag
// wins, then (with --config) what the server's own config stack gives, then
// env LOOMCYCLE_PG_DSN alone. The verbs accept --config OR --dsn; --dsn is the
// lower-friction path for one-off ops without a yaml in CWD.
//
// With --config the stack is the server's: presets, the config dir, each
// named file with its section files, and the auth.env beside the last file.
// The DSN can therefore come from a place the named file does not show, so
// before the caller acts this prints, to stderr, the layers read and where
// the DSN was taken from. It never prints the DSN or any part of it.
func loadPgDSN(cfgPaths []string, explicitDSN string, stderr io.Writer) (string, error) {
	const from = "migrate: Postgres DSN taken from: %s\n"
	if explicitDSN != "" {
		fmt.Fprintf(stderr, from, "--dsn")
		return explicitDSN, nil
	}
	if len(cfgPaths) > 0 {
		// Read before the assembly: it loads auth.env into the environment.
		envHadDSN := strings.TrimSpace(config.PgDSNFromEnv()) != ""
		stack, err := assembleForCLI(cfgPaths...)
		if err != nil {
			return "", fmt.Errorf("config: %w", err)
		}
		layers := describeStack(stack)
		fmt.Fprintf(stderr, "migrate: config layers: %s\n", layers)
		cfg, err := config.LoadLayers(stack.Layers...)
		if err != nil {
			return "", fmt.Errorf("config: %w", err)
		}
		if cfg.Storage.Backend != "postgres" {
			return "", fmt.Errorf("storage.backend is %q (want \"postgres\") in the config layers read (%s)", cfg.Storage.Backend, layers)
		}
		if cfg.Storage.PgDSN == "" {
			return "", fmt.Errorf("no layer read (%s) gives a storage.pg_dsn and LOOMCYCLE_PG_DSN is unset", layers)
		}
		source, err := pgDSNSource(stack, envHadDSN)
		if err != nil {
			return "", fmt.Errorf("config: %w", err)
		}
		fmt.Fprintf(stderr, from, source)
		return cfg.Storage.PgDSN, nil
	}
	if v := config.PgDSNFromEnv(); v != "" {
		fmt.Fprintf(stderr, from, "the environment (LOOMCYCLE_PG_DSN)")
		return v, nil
	}
	return "", fmt.Errorf("no DSN: pass --dsn, --config <yaml>, or set LOOMCYCLE_PG_DSN")
}

// pgDSNSource says where a loaded config's DSN came from, in the loader's
// order: LOOMCYCLE_PG_DSN wins when it is set, and it is either the process
// environment's or the auth.env's (which only sets what the environment
// lacked); otherwise the last yaml layer that names storage.pg_dsn.
func pgDSNSource(stack ConfigStack, envHadDSN bool) (string, error) {
	if strings.TrimSpace(config.PgDSNFromEnv()) != "" {
		if envHadDSN || stack.AuthEnvPath == "" {
			return "the environment (LOOMCYCLE_PG_DSN)", nil
		}
		return "auth.env (" + stack.AuthEnvPath + ")", nil
	}
	layer, err := config.PgDSNLayer(stack.Layers)
	if err != nil {
		return "", err
	}
	if layer == "" {
		return "a source this command cannot name", nil
	}
	return "storage.pg_dsn in " + layer, nil
}

func runMigrateUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPaths := configFlag(fs, "path to a config YAML, as the server is given it (repeatable, layered in the order given; alternative to --dsn)")
	dsn := fs.String("dsn", "", "Postgres DSN (overrides yaml/env)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedDSN, err := loadPgDSN(*cfgPaths, *dsn, stderr)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	if err := storepostgres.MigrateUp(resolvedDSN); err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: migrate up: %v\n", err)
		return 1
	}
	// memory.fulltext rebuilds a column the migrations own, so it may need the
	// same role. Only with --config: --dsn alone carries no setting, and
	// applying the default would undo the server's.
	if len(*cfgPaths) > 0 {
		cfg, err := loadLayeredConfig(*cfgPaths...)
		if err != nil {
			return fail(stderr, "config: %v", err)
		}
		ft := storepostgres.FullTextConfig{Config: cfg.Memory.FullText.Config, Unaccent: cfg.Memory.FullText.Unaccent}
		applied, err := storepostgres.ApplyFullTextConfig(context.Background(), resolvedDSN, ft)
		if err != nil {
			fmt.Fprintf(stderr, "loomcycle: error: migrate up: memory.fulltext: %v\n", err)
			return 1
		}
		if applied {
			fmt.Fprintf(stdout, "memory.fulltext: config=%s unaccent=%v\n", cfg.Memory.FullText.EffectiveConfig(), ft.Unaccent)
		}
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
	cfgPaths := configFlag(fs, "path to a config YAML, as the server is given it (repeatable, layered in the order given; alternative to --dsn)")
	dsn := fs.String("dsn", "", "Postgres DSN (overrides yaml/env)")
	confirm := fs.Bool("yes", false, "confirm the destructive operation (down rolls every migration back)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*confirm {
		fmt.Fprintln(stderr, "migrate down rolls every applied migration back — pass --yes to confirm.")
		return 2
	}
	resolvedDSN, err := loadPgDSN(*cfgPaths, *dsn, stderr)
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
	cfgPaths := configFlag(fs, "path to a config YAML, as the server is given it (repeatable, layered in the order given; alternative to --dsn)")
	dsn := fs.String("dsn", "", "Postgres DSN (overrides yaml/env)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedDSN, err := loadPgDSN(*cfgPaths, *dsn, stderr)
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
