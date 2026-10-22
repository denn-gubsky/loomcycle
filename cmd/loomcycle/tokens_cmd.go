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
	"strconv"
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

const tokensRewrapUsage = "Usage: loomcycle tokens rewrap [--config <yaml>]... [--preset <name>]... [--def-id <def_id>@<fingerprint>]... | [--all-listed <digest>] [--dry-run]"

// runTokensRewrap moves the operator-token rows minted before the derived
// pepper onto it (RFC ES C2), in two steps. Without --def-id or --all-listed
// it only lists the live unpeppered rows, with what each grants, a
// fingerprint per row and a digest of the set; it writes nothing. Writing
// needs the operator to name the rows they recognise, pinned to what was
// reviewed. A row nobody named is never rewrapped, so a database writer who
// inserts one gains nothing by waiting for this command.
func runTokensRewrap(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tokens rewrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfgPaths, presets, named []string
	fs.Func("config", "path to config YAML, as the server is given it (repeatable; default: the server's auto-discovery)", func(v string) error {
		cfgPaths = append(cfgPaths, v)
		return nil
	})
	fs.Func("preset", "embedded preset/bundle, as the server is given it (repeatable; default: LOOMCYCLE_PRESETS)", func(v string) error {
		presets = append(presets, v)
		return nil
	})
	fs.Func("def-id", "rewrap this reviewed row, given as <def_id>@<fingerprint> from the review (repeatable)", func(v string) error {
		if v = strings.TrimSpace(v); v == "" {
			return errors.New("empty --def-id")
		}
		named = append(named, v)
		return nil
	})
	digest := fs.String("all-listed", "", "rewrap every row the review listed, given the digest it printed; refused if any row was added, retired or changed since")
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
	if len(named) > 0 && *digest != "" {
		fmt.Fprintln(stderr, "loomcycle: error: name rows with --def-id or --all-listed, not both")
		return 2
	}

	layers, err := assembleConfigLayers(presets, cfgPaths, false)
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
	res, err := lchttp.ListUntaggedTokenRows(ctx, st, h)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: reading the token table: %v\n", err)
		return 1
	}
	// The pepper fingerprint lets an operator compare this process's key
	// with the server's (its boot log prints the same line), which matters
	// most when no tagged row exists yet to give a wrong key away.
	fmt.Fprintf(stdout, "derived pepper fingerprint: %s (the server's boot log prints its own; they must match)\n", h.Fingerprint())
	fmt.Fprintf(stdout, "live token rows: %d unpeppered, %d current, %d wrapped, %d under LOOMCYCLE_SECRET_KEY_PREVIOUS, %d under another key; %d unpeppered retired or expired\n",
		len(res.Live), res.Current, res.Wrapped, res.Previous, res.OtherKey, len(res.Dead))
	if res.LikelyWrongKey() {
		fmt.Fprintf(stderr, "loomcycle: error: %d live token row(s) are hashed under another LOOMCYCLE_SECRET_KEY and none under this one (pepper fingerprint %s): this is probably not the server's key, and wrapping under it would make the tokens unusable. Run with the server's LOOMCYCLE_SECRET_KEY (compare the fingerprint with the server's boot log). If this is the server's key, those rows cannot log in: retire them, or set their key as LOOMCYCLE_SECRET_KEY_PREVIOUS on the server.\n",
			res.OtherKey, h.Fingerprint())
		return 2
	}

	if len(named) == 0 && *digest == "" {
		printTokenReview(stdout, res)
		return 0
	}
	picked, err := lchttp.SelectUntaggedTokenRows(res.Live, named, *digest)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: %v; nothing was rewrapped\n", err)
		return 2
	}
	if *dryRun {
		fmt.Fprintf(stdout, "dry run: would rewrap %d row(s); nothing was changed:\n", len(picked))
		for _, r := range picked {
			printTokenRow(stdout, r)
		}
		return 0
	}
	moved, notMoved, err := lchttp.RewrapTokenRows(ctx, st, h, picked)
	if err != nil {
		fmt.Fprintf(stderr, "loomcycle: error: rewrap: %v (nothing was rewrapped)\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "rewrapped %d of %d named row(s):\n", len(moved), len(picked))
	for _, r := range moved {
		printTokenRow(stdout, r)
	}
	if left := len(res.Live) - len(picked); left > 0 {
		fmt.Fprintf(stdout, "%d unpeppered row(s) you did not name were left as they are; they still cannot log in\n", left)
	}
	if len(moved) > 0 {
		fmt.Fprintln(stdout, "Each rewrapped token logs in again and moves onto the current form at its next login. A row minted under a since-removed LOOMCYCLE_OPERATOR_TOKEN_PEPPER stays invalid; re-mint it.")
	}
	if len(notMoved) > 0 {
		fmt.Fprintf(stdout, "NOT moved — changed after they were read (another rewrap running at the same time, or a database edit), left as they are:\n")
		for _, r := range notMoved {
			fmt.Fprintf(stdout, "  %q\n", r.DefID)
		}
		fmt.Fprintf(stderr, "loomcycle: error: %d named row(s) were not moved; run the review again\n", len(notMoved))
		return 1
	}
	return 0
}

// printTokenRow prints one row as the review shows it. Every field comes
// from the database, so a forged row chooses it: each is printed quoted,
// with control and non-printable characters escaped, so no row can redraw
// the terminal to hide itself or fake another.
func printTokenRow(w io.Writer, r store.OperatorTokenDefRow) {
	fmt.Fprintf(w, "  %q\n", r.DefID)
	fmt.Fprintf(w, "      name: %q   tenant: %q   subject: %q\n", r.Name, r.TenantID, r.Subject)
	quoted := make([]string, 0, len(r.AllowedScopes))
	for _, sc := range r.AllowedScopes {
		quoted = append(quoted, strconv.Quote(sc))
	}
	scopes := strings.Join(quoted, ", ")
	if scopes == "" {
		scopes = "(none)"
	}
	fmt.Fprintf(w, "      scopes: %s\n", scopes)
	times := "created " + r.CreatedAt.UTC().Format(time.RFC3339)
	if !r.RetiredAt.IsZero() {
		times += "   retired " + r.RetiredAt.UTC().Format(time.RFC3339)
	}
	if !r.ExpiresAt.IsZero() {
		times += "   expires " + r.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(w, "      %s\n", times)
	if slices.Contains(r.AllowedScopes, auth.ScopeAdmin) {
		fmt.Fprintf(w, "      !!! ADMIN TOKEN (%s): full control of every tenant. Be certain you minted it.\n", auth.ScopeAdmin)
	}
	if lchttp.TokenRowNonASCII(r) {
		fmt.Fprintln(w, "      ! non-ASCII characters in the name, tenant or subject: check them against what you minted (lookalike letters).")
	}
	if lchttp.SuspiciousTokenRow(r) {
		fmt.Fprintln(w, "      !!! SUSPICIOUS: no mint writes this row (an odd def_id, or control or non-printable characters). It cannot be rewrapped.")
		return
	}
	fmt.Fprintf(w, "      fingerprint: %s   to rewrap: --def-id %s@%s\n", lchttp.TokenRowFingerprint(r), r.DefID, lchttp.TokenRowFingerprint(r))
}

// printTokenReview lists each live unpeppered row with what it grants, so the
// operator can tell a token they minted from a row someone wrote into the
// database, and how to confirm the ones they recognise.
func printTokenReview(w io.Writer, res lchttp.UntaggedTokenRows) {
	if len(res.Dead) > 0 {
		fmt.Fprintf(w, "\n%d retired or expired unpeppered row(s), not rewrappable (they can never log in):\n", len(res.Dead))
		for _, r := range res.Dead {
			fmt.Fprintf(w, "  %q\n", r.DefID)
		}
	}
	if len(res.Live) == 0 {
		fmt.Fprintln(w, "nothing to rewrap")
		return
	}
	fmt.Fprintf(w, "\n%d unpeppered row(s) cannot log in until rewrapped:\n\n", len(res.Live))
	suspicious := 0
	for _, r := range res.Live {
		printTokenRow(w, r)
		if lchttp.SuspiciousTokenRow(r) {
			suspicious++
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A def_id you never minted was written into the database: do not name it here. Retire it")
	fmt.Fprintln(w, "(loomcycle operator-token retire --def-id <id>, or set retired_at on the row in the database);")
	fmt.Fprintln(w, "a retired row leaves the rewrap set and can never be rewrapped.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "To rewrap the rows you recognise, run again with the same config and environment, naming each")
	fmt.Fprintln(w, "as printed above:")
	fmt.Fprintln(w, "  loomcycle tokens rewrap --def-id <def_id>@<fingerprint> [--def-id ...]")
	if suspicious > 0 {
		fmt.Fprintf(w, "%d row(s) above are SUSPICIOUS: --all-listed is refused while they are live. Name the rows you minted with --def-id instead, or retire the suspicious ones and review again.\n", suspicious)
	}
	// The count right before the digest: a row hidden from the listing
	// would show as a count that does not match the rows above.
	fmt.Fprintf(w, "or, to rewrap exactly the %d row(s) listed above (refused if any was added, retired or changed since):\n", len(res.Live))
	fmt.Fprintf(w, "  loomcycle tokens rewrap --all-listed %s\n", lchttp.UntaggedTokenSetDigest(res.Live))
	fmt.Fprintln(w, "nothing was changed")
}
