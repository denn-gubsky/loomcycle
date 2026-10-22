package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runTokens is `loomcycle tokens <verb>`. It lives here rather than in
// internal/cli because it opens the store exactly as the server does: the
// same config layering (presets, CONFIG_DIR, CONFIG_FILES, --config, the
// XDG default, auth.env) and the same openStore. A pepper or a database
// different from the server's would list the wrong rows, or wrap them under
// the wrong key.
//
// Exit codes match the other CLIs: 0 ok, 1 operational failure, 2 a usage
// or configuration error, refusals included.
func runTokens(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "rewrap" {
		fmt.Fprintln(stderr, tokensRewrapUsage)
		return 2
	}
	return runTokensRewrap(args[1:], stdout, stderr)
}

const tokensRewrapUsage = "Usage: loomcycle tokens rewrap [--config <yaml>]... [--def-id <id>]... | [--all-listed <digest>] [--dry-run]"

// runTokensRewrap moves the operator-token rows minted before the derived
// pepper onto it (RFC ES C2), in two steps. Without --def-id or --all-listed
// it only lists the unpeppered rows, with what each grants and a digest of
// the set; it writes nothing. Writing needs the operator to name the rows
// they recognise. A row nobody named is never rewrapped, so a database
// writer who inserts one gains nothing by waiting for this command.
func runTokensRewrap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tokens rewrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfgPaths, defIDs []string
	fs.Func("config", "path to config YAML, as the server is given it (repeatable; default: the server's auto-discovery)", func(v string) error {
		cfgPaths = append(cfgPaths, v)
		return nil
	})
	fs.Func("def-id", "rewrap this listed row (repeatable); every one named must be an unpeppered row now", func(v string) error {
		if v = strings.TrimSpace(v); v == "" {
			return errors.New("empty def_id")
		}
		defIDs = append(defIDs, v)
		return nil
	})
	digest := fs.String("all-listed", "", "rewrap every row the listing showed, given the digest it printed; refused if any row was added or changed since")
	dryRun := fs.Bool("dry-run", false, "with --def-id or --all-listed: check the selection and show what would be rewrapped, writing nothing")
	fs.Usage = func() {
		fmt.Fprintln(stderr, tokensRewrapUsage)
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Moves operator tokens minted before LOOMCYCLE_SECRET_KEY was set onto the pepper")
		fmt.Fprintln(stderr, "derived from it, so they log in again. Run it with the server's own config and")
		fmt.Fprintln(stderr, "environment. Without --def-id or --all-listed it only lists the rows: review them,")
		fmt.Fprintln(stderr, "then name the ones you minted. Safe while servers are running.")
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
	*digest = strings.TrimSpace(*digest)
	if len(defIDs) > 0 && *digest != "" {
		fmt.Fprintln(stderr, "loomcycle: error: name rows with --def-id or --all-listed, not both")
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

	ctx := context.Background()
	rep, rows, err := lchttp.ListUntaggedTokenRows(ctx, st, h)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: reading the token table: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "token rows under the derived pepper: %d unpeppered, %d wrapped, %d under LOOMCYCLE_SECRET_KEY_PREVIOUS, %d under another key\n",
		len(rep.Untagged), rep.Wrapped, rep.Previous, rep.OtherKey)

	if len(defIDs) == 0 && *digest == "" {
		printTokenReview(stdout, rows)
		return 0
	}
	picked, err := lchttp.SelectUntaggedTokenRows(rows, defIDs, *digest)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: %v; nothing was rewrapped\n", err)
		return 2
	}
	if *dryRun {
		fmt.Fprintf(stdout, "dry run: would rewrap %d row(s); nothing was changed:\n", len(picked))
		printTokenRowIDs(stdout, picked)
		return 0
	}
	n, err := lchttp.RewrapTokenRows(ctx, st, h, picked)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: rewrap: %v (nothing was rewrapped)\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "rewrapped %d of %d named row(s):\n", n, len(picked))
	printTokenRowIDs(stdout, picked)
	if left := len(rows) - len(picked); left > 0 {
		fmt.Fprintf(stdout, "%d unpeppered row(s) you did not name were left as they are; they still cannot log in\n", left)
	}
	fmt.Fprintln(stdout, "Each rewrapped token logs in again and moves onto the current form at its next login. A row minted under a since-removed LOOMCYCLE_OPERATOR_TOKEN_PEPPER stays invalid; re-mint it.")
	if n < len(picked) {
		fmt.Fprintf(stderr, "loomcycle: error: %d named row(s) changed after they were read (another rewrap running at the same time, or a database edit) and were left as they are; run the review again\n", len(picked)-n)
		return 1
	}
	return 0
}

// printTokenReview lists each unpeppered row with what it grants, so the
// operator can tell a token they minted from a row someone wrote into the
// database, and how to confirm the ones they recognise.
func printTokenReview(w io.Writer, rows []store.OperatorTokenDefRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "nothing to rewrap")
		return
	}
	fmt.Fprintf(w, "\n%d unpeppered row(s) cannot log in until rewrapped:\n\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(w, "  %s\n", r.DefID)
		fmt.Fprintf(w, "      name: %s   tenant: %s   subject: %s\n", r.Name, r.TenantID, r.Subject)
		scopes := strings.Join(r.AllowedScopes, ", ")
		if scopes == "" {
			scopes = "(none)"
		}
		fmt.Fprintf(w, "      scopes: %s\n", scopes)
		created := "created " + r.CreatedAt.UTC().Format(time.RFC3339)
		if !r.RetiredAt.IsZero() {
			created += "   retired " + r.RetiredAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "      %s\n", created)
		if slices.Contains(r.AllowedScopes, auth.ScopeAdmin) {
			fmt.Fprintf(w, "      !!! ADMIN TOKEN (%s): full control of every tenant. Be certain you minted it.\n", auth.ScopeAdmin)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A def_id you never minted was written into the database: do not name it here; retire it instead")
	fmt.Fprintln(w, "(loomcycle operator-token retire --def-id <id>).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "To rewrap the rows you recognise, run again with the same config and environment, naming them:")
	fmt.Fprintln(w, "  loomcycle tokens rewrap --def-id <id> [--def-id <id> ...]")
	fmt.Fprintln(w, "or, to rewrap exactly the rows listed above (refused if any was added or changed since):")
	fmt.Fprintf(w, "  loomcycle tokens rewrap --all-listed %s\n", lchttp.UntaggedTokenSetDigest(rows))
	fmt.Fprintln(w, "nothing was changed")
}

func printTokenRowIDs(w io.Writer, rows []store.OperatorTokenDefRow) {
	for _, r := range rows {
		fmt.Fprintf(w, "  %s\n", r.DefID)
	}
}
