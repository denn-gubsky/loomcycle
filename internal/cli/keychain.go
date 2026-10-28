package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Where `loomcycle login` keeps the token it was issued (RFC FA §7): the
// operating system's credential store, and nowhere else — never a dotfile,
// never a config file, never an environment file. A token in a file is read by
// every process of the user, every backup and every editor plugin; the
// keychain asks the OS to hold it.
//
// Both supported stores are driven through the tool the platform already
// ships, so no new module and no cgo:
//
//   - macOS: /usr/bin/security, the login keychain. The token goes to
//     `security -i` on STDIN, never on a command line, where any local user
//     could read it from the process list for as long as the command runs.
//   - Linux: secret-tool (libsecret), the Secret Service (GNOME Keyring,
//     KWallet, KeePassXC). `secret-tool store` reads the secret from stdin.
//   - Windows and others: not supported. DPAPI or the Credential Manager would
//     need syscalls this package does not carry; such a user sets
//     LOOMCYCLE_MCP_UPSTREAM_TOKEN (or passes --token) instead, and login says
//     so rather than falling back to a file.

// keychainService names loomcycle's items in the store; the account is the
// server's canonical URL, so each server gets its own token.
const keychainService = "loomcycle"

var (
	// errNoStoredToken: the store has no token for this server.
	errNoStoredToken = errors.New("no token stored for this server")
	// errNoKeychain: this machine has no credential store login can use.
	errNoKeychain = errors.New("no OS keychain available")
)

// tokenStore is the storage abstraction login, logout, auth status and the MCP
// thin client share. Every implementation keeps the token in an OS credential
// store; there is deliberately no file-backed one.
type tokenStore interface {
	// Name is the store's human name, for messages.
	Name() string
	// Get returns the token stored for server, errNoStoredToken when none.
	Get(ctx context.Context, server string) (string, error)
	// Set stores (or replaces) the token for server.
	Set(ctx context.Context, server, token string) error
	// Delete removes the token for server; nil when there was none.
	Delete(ctx context.Context, server string) error
}

// runResult is what one store command produced.
type runResult struct {
	stdout, stderr string
	exitCode       int
}

// commandRunner runs a store tool with stdin. Injected so tests can see every
// argument a store passes (the token must never be one) without touching the
// developer's real keychain.
type commandRunner func(ctx context.Context, name string, args []string, stdin string) (runResult, error)

func execRunner(ctx context.Context, name string, args []string, stdin string) (runResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := runResult{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.exitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// osTokenStore returns this platform's credential store, or errNoKeychain
// (wrapped with what to do) when there is none.
func osTokenStore() (tokenStore, error) {
	return newOSTokenStore(runtime.GOOS, exec.LookPath, execRunner)
}

func newOSTokenStore(goos string, lookPath func(string) (string, error), run commandRunner) (tokenStore, error) {
	switch goos {
	case "darwin":
		p, err := lookPath("security")
		if err != nil {
			return nil, fmt.Errorf("%w: the macOS `security` tool is not on PATH", errNoKeychain)
		}
		return &macKeychain{tool: p, run: run}, nil
	case "linux", "freebsd", "openbsd", "netbsd":
		p, err := lookPath("secret-tool")
		if err != nil {
			return nil, fmt.Errorf("%w: install secret-tool (libsecret-tools) and run a Secret Service "+
				"(GNOME Keyring, KWallet or KeePassXC), or set LOOMCYCLE_MCP_UPSTREAM_TOKEN instead", errNoKeychain)
		}
		return &secretTool{tool: p, run: run}, nil
	}
	return nil, fmt.Errorf("%w: storing a login token is not supported on %s; set LOOMCYCLE_MCP_UPSTREAM_TOKEN "+
		"(or pass --token) instead", errNoKeychain, goos)
}

// tokenShapeRE is what a loomcycle bearer looks like (lct_ + base58, or an
// operator's own secret of URL-safe characters). Anything else is refused
// before it reaches a store command, so a token can never break out of the
// quoted argument macOS's `security -i` parses.
var tokenShapeRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,512}$`)

// canonicalServer is the account a server's token is stored under: the
// scheme and host lower-cased, the path kept without a trailing slash, no
// query, fragment or user info.
func canonicalServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%q is not an http(s) server URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q: a server URL has no user info, query or fragment", raw)
	}
	out := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	if strings.ContainsAny(out, "'\"\\ \t\r\n") {
		return "", fmt.Errorf("%q: unsupported characters in the server URL", raw)
	}
	return out, nil
}

// ---- macOS ------------------------------------------------------------------

type macKeychain struct {
	tool string
	run  commandRunner
}

func (*macKeychain) Name() string { return "the macOS Keychain" }

// macItemNotFound is security(1)'s exit status for errSecItemNotFound.
const macItemNotFound = 44

func (k *macKeychain) Get(ctx context.Context, server string) (string, error) {
	res, err := k.run(ctx, k.tool, []string{"find-generic-password", "-s", keychainService, "-a", server, "-w"}, "")
	if err != nil {
		return "", fmt.Errorf("reading the macOS Keychain: %w", err)
	}
	switch {
	case res.exitCode == macItemNotFound:
		return "", errNoStoredToken
	case res.exitCode != 0:
		return "", fmt.Errorf("reading the macOS Keychain failed (exit %d): %s", res.exitCode, firstLine(res.stderr))
	}
	return strings.TrimRight(res.stdout, "\r\n"), nil
}

func (k *macKeychain) Set(ctx context.Context, server, token string) error {
	if !tokenShapeRE.MatchString(token) {
		return errors.New("refusing to store a token of an unexpected shape")
	}
	// -U updates an existing item. The command line travels on stdin, so the
	// token is in no process's argv.
	cmd := fmt.Sprintf("add-generic-password -U -s '%s' -a '%s' -l 'loomcycle %s' -w '%s'\n",
		keychainService, server, server, token)
	res, err := k.run(ctx, k.tool, []string{"-i"}, cmd)
	if err != nil {
		return fmt.Errorf("writing the macOS Keychain: %w", err)
	}
	// The tool's own output is not quoted: in interactive mode it may echo
	// the command line it failed on, and that line holds the token.
	if res.exitCode != 0 {
		return fmt.Errorf("writing the macOS Keychain failed (exit %d)", res.exitCode)
	}
	return verifyStored(ctx, k, server, token)
}

func (k *macKeychain) Delete(ctx context.Context, server string) error {
	res, err := k.run(ctx, k.tool, []string{"delete-generic-password", "-s", keychainService, "-a", server}, "")
	if err != nil {
		return fmt.Errorf("deleting from the macOS Keychain: %w", err)
	}
	if res.exitCode != 0 && res.exitCode != macItemNotFound {
		return fmt.Errorf("deleting from the macOS Keychain failed (exit %d): %s", res.exitCode, firstLine(res.stderr))
	}
	return nil
}

// ---- Secret Service (Linux) -------------------------------------------------

type secretTool struct {
	tool string
	run  commandRunner
}

func (*secretTool) Name() string { return "the Secret Service keyring" }

func (k *secretTool) attrs(server string) []string {
	return []string{"service", keychainService, "account", server}
}

func (k *secretTool) Get(ctx context.Context, server string) (string, error) {
	res, err := k.run(ctx, k.tool, append([]string{"lookup"}, k.attrs(server)...), "")
	if err != nil {
		return "", fmt.Errorf("reading the Secret Service: %w", err)
	}
	// secret-tool exits 1 with no output for "no such item", and 1 with a
	// message for a store it cannot reach.
	if res.exitCode != 0 {
		if strings.TrimSpace(res.stderr) == "" {
			return "", errNoStoredToken
		}
		return "", fmt.Errorf("reading the Secret Service failed: %s", firstLine(res.stderr))
	}
	tok := strings.TrimRight(res.stdout, "\r\n")
	if tok == "" {
		return "", errNoStoredToken
	}
	return tok, nil
}

func (k *secretTool) Set(ctx context.Context, server, token string) error {
	if !tokenShapeRE.MatchString(token) {
		return errors.New("refusing to store a token of an unexpected shape")
	}
	args := append([]string{"store", "--label", "loomcycle " + server}, k.attrs(server)...)
	res, err := k.run(ctx, k.tool, args, token)
	if err != nil {
		return fmt.Errorf("writing the Secret Service: %w", err)
	}
	if res.exitCode != 0 {
		return fmt.Errorf("writing the Secret Service failed: %s", firstLine(res.stderr))
	}
	return verifyStored(ctx, k, server, token)
}

func (k *secretTool) Delete(ctx context.Context, server string) error {
	res, err := k.run(ctx, k.tool, append([]string{"clear"}, k.attrs(server)...), "")
	if err != nil {
		return fmt.Errorf("deleting from the Secret Service: %w", err)
	}
	if res.exitCode != 0 && strings.TrimSpace(res.stderr) != "" {
		return fmt.Errorf("deleting from the Secret Service failed: %s", firstLine(res.stderr))
	}
	return nil
}

// verifyStored reads the token back: a store tool that reports success but
// kept nothing (a locked keychain, a Secret Service that went away) must not
// leave login claiming the person is signed in.
func verifyStored(ctx context.Context, s tokenStore, server, token string) error {
	got, err := s.Get(ctx, server)
	if err != nil {
		return fmt.Errorf("the token did not read back from %s: %w", s.Name(), err)
	}
	if got != token {
		return fmt.Errorf("the token read back from %s does not match what was stored", s.Name())
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
