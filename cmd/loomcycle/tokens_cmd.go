package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// runTokens is `loomcycle tokens <verb>`. It lives here rather than in
// internal/cli because it opens the store exactly as the server does: the
// same config layering (presets, CONFIG_DIR, CONFIG_FILES, --config, the
// XDG default, auth.env) and the same openStore. A pepper or a database
// different from the server's would rewrap the wrong rows, or under the
// wrong key.
//
// Exit codes match the other CLIs: 0 ok, 1 operational failure, 2 a usage
// or configuration error, refusals included.
func runTokens(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "rewrap" {
		fmt.Fprintln(stderr, "Usage: loomcycle tokens rewrap [--config <yaml>]... [--dry-run]")
		return 2
	}
	return runTokensRewrap(args[1:], stdout, stderr)
}

// runTokensRewrap moves the operator-token rows minted before the derived
// pepper onto it (RFC ES C2). Boot only reports those rows; this is the one
// place that rewraps them, on the operator's say-so.
func runTokensRewrap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tokens rewrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfgPaths []string
	fs.Func("config", "path to config YAML, as the server is given it (repeatable; default: the server's auto-discovery)", func(v string) error {
		cfgPaths = append(cfgPaths, v)
		return nil
	})
	dryRun := fs.Bool("dry-run", false, "list the rows that would be rewrapped and change nothing")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: loomcycle tokens rewrap [--config <yaml>]... [--dry-run]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Moves operator tokens minted before LOOMCYCLE_SECRET_KEY was set onto the pepper")
		fmt.Fprintln(stderr, "derived from it, so they log in again. Run it once after upgrading, with the")
		fmt.Fprintln(stderr, "server's own config and environment. Safe while servers are running.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "loomcycle: error: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	layers, err := assembleConfigLayers(nil, cfgPaths, false)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: %v\n", err)
		return 2
	}
	cfg, err := config.LoadLayers(layers...)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: config: %v\n", err)
		return 2
	}
	h, err := cfg.Env.TokenHasher()
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: token pepper: %v\n", err)
		return 2
	}
	switch h.Source() {
	case auth.PepperExplicit:
		fmt.Fprintln(stderr, "loomcycle: error: LOOMCYCLE_OPERATOR_TOKEN_PEPPER is set, so tokens are hashed under that explicit pepper and every stored hash is already current. There is nothing to rewrap.")
		return 2
	case auth.PepperNone:
		fmt.Fprintln(stderr, "loomcycle: error: no token pepper: LOOMCYCLE_SECRET_KEY is not set (nor LOOMCYCLE_OPERATOR_TOKEN_PEPPER), so there is no pepper to rewrap under and the stored hashes are already current. If the server runs with LOOMCYCLE_SECRET_KEY, run this command with the same environment.")
		return 2
	}
	// A sqlite path that does not exist means this is not the server's
	// database (wrong LOOMCYCLE_DATA_DIR or config); opening it would create
	// an empty one and report nothing to do.
	if cfg.Storage.Backend == "sqlite" || cfg.Storage.Backend == "" {
		dbPath := filepath.Join(cfg.Env.DataDir, "loomcycle.db")
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "loomcycle: error: no database at %s: run with the server's LOOMCYCLE_DATA_DIR and config\n", dbPath)
			return 2
		}
	}
	st, closeStore, err := openStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: store: %v\n", err)
		return 1
	}
	defer closeStore()

	res, err := lchttp.RewrapUntaggedTokenHashes(context.Background(), st, h, *dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: rewrap: %v (nothing was rewrapped)\n", err)
		return 1
	}
	printTokenRewrap(stdout, res, *dryRun)
	return 0
}

func printTokenRewrap(w io.Writer, res lchttp.TokenRewrapResult, dryRun bool) {
	fmt.Fprintf(w, "token rows under the derived pepper: %d unpeppered, %d wrapped, %d under LOOMCYCLE_SECRET_KEY_PREVIOUS, %d under another key\n",
		len(res.Untagged), res.Wrapped, res.Previous, res.OtherKey)
	if len(res.Untagged) == 0 {
		fmt.Fprintln(w, "nothing to rewrap")
		return
	}
	if dryRun {
		fmt.Fprintf(w, "dry run: would rewrap %d row(s); nothing was changed:\n", len(res.Untagged))
	} else {
		fmt.Fprintf(w, "rewrapped %d of %d row(s):\n", res.Rewrapped, len(res.Untagged))
	}
	for _, id := range res.Untagged {
		fmt.Fprintf(w, "  %s\n", id)
	}
	if !dryRun && res.Rewrapped < len(res.Untagged) {
		fmt.Fprintf(w, "%d row(s) changed after the scan (most likely another rewrap running at the same time) and were left as they are\n",
			len(res.Untagged)-res.Rewrapped)
	}
	fmt.Fprintln(w, "A def_id you never minted is a row someone wrote into the database: retire it (loomcycle operator-token retire --def-id <id>).")
	if !dryRun {
		fmt.Fprintln(w, "Each rewrapped token logs in again and moves onto the current form at its next login. A row minted under a since-removed LOOMCYCLE_OPERATOR_TOKEN_PEPPER stays invalid; re-mint it.")
	}
}
