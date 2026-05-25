package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	oauthdev "github.com/denn-gubsky/loomcycle/internal/providers/anthropic_oauth_dev"
)

// RunAnthropic dispatches to one of:
//
//	loomcycle anthropic login           one-time OAuth login (opens browser)
//	loomcycle anthropic status          print current token state
//	loomcycle anthropic logout          delete the local token file
//
// All three are gated on LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 — when
// the env var is unset (or any value other than "1"), every subcommand
// prints a clear error pointing at the env var + the docs section.
//
// Exit codes mirror the rest of the CLI surface: 0 on success, 2 on
// user error (missing flags, unknown verb, env-var not set), 1 on
// operational failure (token endpoint returned 4xx/5xx, network
// unreachable, file write failure).
func RunAnthropic(args []string, stdout, stderr io.Writer) int {
	if !oauthDevEnabled() {
		fmt.Fprintf(stderr, `loomcycle anthropic: subcommands require %s=1 to be set.

The OAuth-dev provider is opt-in (research/load-testing only; NOT for
production deployment). See docs/PROVIDERS.md for the full operator
walkthrough + risk acknowledgement.
`, oauthdev.EnvEnabled)
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: loomcycle anthropic {login|status|logout}")
		return 2
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "login":
		return runAnthropicLogin(rest, stdout, stderr)
	case "status":
		return runAnthropicStatus(rest, stdout, stderr)
	case "logout":
		return runAnthropicLogout(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "loomcycle anthropic: unknown subcommand %q (want login|status|logout)\n", verb)
		return 2
	}
}

// runAnthropicLogin runs the PKCE flow: opens a localhost callback
// server, opens the operator's browser at the authorize URL, waits
// for the callback, exchanges the code for tokens, persists them.
//
// Operator can pass --manual to skip the auto-open (useful when
// xdg-open / open / cmd start isn't wired up correctly OR when the
// operator wants to inspect the URL before opening it). The browser
// MUST be on the same machine as loomcycle — the callback server
// binds to 127.0.0.1, so a browser on a different machine can't
// reach it. Cross-machine login is not supported in v0.11.9.
func runAnthropicLogin(args []string, stdout, stderr io.Writer) int {
	manual := false
	for _, a := range args {
		switch a {
		case "--manual":
			manual = true
		default:
			fmt.Fprintf(stderr, "login: unknown flag %q\n", a)
			return 2
		}
	}

	// Print the disclaimer BEFORE doing any auth work so the operator
	// sees the risk acknowledgement framing every time they re-run
	// login (token refresh, version bumps, etc.). The env-var gate
	// already serves as the formal opt-in; this is a visible reminder
	// of what that opt-in means.
	printOAuthDevDisclaimer(stdout)

	pkce, err := oauthdev.NewPKCEPair()
	if err != nil {
		fmt.Fprintf(stderr, "login: PKCE generation failed: %v\n", err)
		return 1
	}

	port := resolveCallbackPort()
	cs, err := oauthdev.StartCallbackServer(port)
	if err != nil {
		fmt.Fprintf(stderr, "login: callback server failed: %v\n", err)
		fmt.Fprintf(stderr, "  hint: another process may be bound to port %d. Override with %s=<port>.\n",
			port, oauthdev.EnvCallbackPort)
		return 1
	}
	defer cs.Close()

	authURL := oauthdev.BuildAuthorizeURL(pkce, cs.Port())
	if manual {
		fmt.Fprintln(stdout, "Open this URL in your browser to authorize loomcycle:")
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "  "+authURL)
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "NOTE: the browser must be on THIS machine — the callback server")
		fmt.Fprintln(stdout, "binds to 127.0.0.1, so a browser on a different host cannot reach it.")
		fmt.Fprintln(stdout, "")
		fmt.Fprintf(stdout, "Listening for callback on http://127.0.0.1:%d/callback (will timeout in 5 min)...\n", cs.Port())
	} else {
		fmt.Fprintln(stdout, "Opening your browser to authorize loomcycle…")
		fmt.Fprintln(stdout, "(if nothing opens, re-run with `--manual` and copy the URL)")
		if err := openBrowser(authURL); err != nil {
			fmt.Fprintf(stderr, "login: failed to open browser (%v) — try `--manual`\n", err)
			return 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := cs.WaitFor(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "login: callback wait failed: %v\n", err)
		return 1
	}
	if result.Err != nil {
		fmt.Fprintf(stderr, "login: %v\n", result.Err)
		return 1
	}
	if result.State != pkce.Verifier {
		fmt.Fprintln(stderr, "login: state/verifier mismatch — possible CSRF; refuse to continue")
		return 1
	}
	tok, err := oauthdev.ExchangeCodeForToken(ctx, result.Code, result.State, pkce.Verifier, cs.Port(), oauthdev.ExchangeOptions{})
	if err != nil {
		fmt.Fprintf(stderr, "login: token exchange failed: %v\n", err)
		return 1
	}

	storePath, err := oauthdev.DefaultTokenStorePath()
	if err != nil {
		fmt.Fprintf(stderr, "login: cannot resolve config dir: %v\n", err)
		return 1
	}
	store := oauthdev.NewTokenStore(storePath)
	if err := store.Save(tok); err != nil {
		fmt.Fprintf(stderr, "login: save token failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ logged in — tokens saved to %s\n", storePath)
	fmt.Fprintf(stdout, "  access expires: %s (%v from now)\n",
		tok.ExpiresAt.Format(time.RFC3339), time.Until(tok.ExpiresAt).Round(time.Second))
	fmt.Fprintf(stdout, "  scope: %s\n", tok.Scope)
	return 0
}

// runAnthropicStatus prints the on-disk token state. Returns 0 even
// when no token exists (logout-status pattern: status is informational,
// not a precondition check).
func runAnthropicStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "status: takes no arguments\n")
		return 2
	}
	storePath, err := oauthdev.DefaultTokenStorePath()
	if err != nil {
		fmt.Fprintf(stderr, "status: cannot resolve config dir: %v\n", err)
		return 1
	}
	store := oauthdev.NewTokenStore(storePath)
	fmt.Fprintf(stdout, "Token storage: %s\n", storePath)
	tok, err := store.Load()
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(stdout, "Logged in:     no")
			fmt.Fprintln(stdout, "Run `loomcycle anthropic login` to authorize.")
			return 0
		}
		fmt.Fprintf(stderr, "status: cannot read token file: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Logged in:     yes")
	expIn := time.Until(tok.ExpiresAt).Round(time.Second)
	if expIn < 0 {
		fmt.Fprintf(stdout, "Access:        EXPIRED %v ago (refresh will run on next background tick)\n", -expIn)
	} else {
		fmt.Fprintf(stdout, "Access expires: %s (in %v)\n", tok.ExpiresAt.Format(time.RFC3339), expIn)
	}
	fmt.Fprintf(stdout, "Scope:         %s\n", tok.Scope)
	fmt.Fprintf(stdout, "Obtained at:   %s\n", tok.ObtainedAt.Format(time.RFC3339))
	if err := store.VerifyPermissions(); err != nil {
		fmt.Fprintf(stdout, "⚠ WARNING:     %v\n", err)
	}
	return 0
}

// runAnthropicLogout deletes the token file. Idempotent — exit 0 even
// when the file was already absent.
func runAnthropicLogout(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "logout: takes no arguments\n")
		return 2
	}
	storePath, err := oauthdev.DefaultTokenStorePath()
	if err != nil {
		fmt.Fprintf(stderr, "logout: cannot resolve config dir: %v\n", err)
		return 1
	}
	store := oauthdev.NewTokenStore(storePath)
	if err := store.Delete(); err != nil {
		fmt.Fprintf(stderr, "logout: delete failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ logged out — %s removed\n", storePath)
	return 0
}

// oauthDevEnabled returns true when LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED
// is set to "1". Any other value (empty, "0", "true", etc.) returns
// false — strict to make the opt-in posture visible.
func oauthDevEnabled() bool {
	return os.Getenv(oauthdev.EnvEnabled) == "1"
}

// resolveCallbackPort honours the env-var override; otherwise uses the
// default. Logged-visible misconfiguration when the env var is set to
// a non-integer value — we use the default in that case rather than
// crash, but stderr gets a notice.
func resolveCallbackPort() int {
	if v := os.Getenv(oauthdev.EnvCallbackPort); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 && n < 65536 {
			return n
		}
		fmt.Fprintf(os.Stderr, "warning: %s=%q is not a valid TCP port; using default %d\n",
			oauthdev.EnvCallbackPort, v, oauthdev.DefaultCallbackPort)
	}
	return oauthdev.DefaultCallbackPort
}

// openBrowser launches the operator's default browser at the URL.
// Cross-platform: `open` on macOS, `xdg-open` on Linux, `cmd /C start`
// on Windows. Returns an error when the platform's opener tool isn't
// in $PATH or the URL fails to launch.
func openBrowser(rawURL string) error {
	// Defence in depth: the rawURL is built by BuildAuthorizeURL from a
	// constant base + url.Values, so injection-via-URL isn't a real
	// risk, but a sanity check on the prefix is cheap insurance against
	// a future refactor passing arbitrary text here.
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://") {
		return fmt.Errorf("refusing to open non-http URL: %q", rawURL)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("cmd", "/C", "start", "", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

// printOAuthDevDisclaimer writes the OAuth-dev risk acknowledgement
// to stdout before the login flow proceeds. The env-var gate
// LOOMCYCLE_ANTHROPIC_OAUTH_DEV_ENABLED=1 is the formal opt-in; this
// disclaimer is the visible reminder of what the opt-in covers. Every
// re-login (token refresh, version bump, etc.) re-prints it — the
// risk doesn't go away after the first login.
//
// The text mirrors docs/PROVIDERS.md's "NO GUARANTEES" lead. Keep
// the two in sync when one is updated.
func printOAuthDevDisclaimer(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "================================================================")
	fmt.Fprintln(w, "  ⚠  loomcycle anthropic-oauth-dev  —  NO GUARANTEES")
	fmt.Fprintln(w, "================================================================")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "The Anthropic OAuth flow this provider uses is NOT an official")
	fmt.Fprintln(w, "integration. It is the product of reverse-engineering done by")
	fmt.Fprintln(w, "the Pi agent team (github.com/earendil-works/pi) and replicated")
	fmt.Fprintln(w, "here by the loomcycle team. We do our best to mimic Claude Code's")
	fmt.Fprintln(w, "wire shape so calls pass through Anthropic's subscription-billing")
	fmt.Fprintln(w, "detection, but we CANNOT GUARANTEE this will continue to work.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "You are running this against YOUR OWN Claude Pro/Max subscription.")
	fmt.Fprintln(w, "Anthropic's subscription terms historically restrict programmatic")
	fmt.Fprintln(w, "use outside the official Anthropic SDK. YOU — the operator — are")
	fmt.Fprintln(w, "solely responsible for any consequences with Anthropic if they")
	fmt.Fprintln(w, "ever object to this use, including account flagging, rate-limiting,")
	fmt.Fprintln(w, "or subscription revocation. loomcycle and its maintainers carry no")
	fmt.Fprintln(w, "warranty, no liability, and provide no support guarantees for")
	fmt.Fprintln(w, "this path. If your account is affected, the resolution is between")
	fmt.Fprintln(w, "you and Anthropic; we cannot intervene.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "If you cannot accept these terms, abort now (Ctrl+C) and use the")
	fmt.Fprintln(w, "production `anthropic` provider (API key) instead.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "See docs/PROVIDERS.md → `anthropic-oauth-dev` for full details.")
	fmt.Fprintln(w, "================================================================")
	fmt.Fprintln(w, "")
}
