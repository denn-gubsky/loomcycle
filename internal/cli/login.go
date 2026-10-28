package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"
)

// RFC FA §7 / §10: the CLI signs a person in with the device flow (RFC 8628)
// and keeps the token the server issues in the OS keychain.
//
//	loomcycle login  [--url <server>]   show a code + link, wait for approval in the browser
//	loomcycle logout [--url <server>]   retire the token on the server, remove it from the keychain
//	loomcycle auth status [--url <server>]
//
// The token is never printed, never written to a file and never taken from the
// command line. The MCP thin client (`loomcycle mcp --upstream <url>`) reads
// the same keychain entry when LOOMCYCLE_MCP_UPSTREAM_TOKEN is not set.

// deviceClientID is how the CLI names itself to the server (shown on the
// approval page).
const deviceClientID = "loomcycle-cli"

// loginDeps are what the login commands touch outside themselves, injectable
// so tests drive them against an httptest server and a fake keychain.
type loginDeps struct {
	store  func() (tokenStore, error)
	client *http.Client
	// sleep waits between polls; it returns early with ctx's error.
	sleep func(ctx context.Context, d time.Duration) error
}

func defaultLoginDeps() loginDeps {
	return loginDeps{
		store:  osTokenStore,
		client: &http.Client{Timeout: 30 * time.Second},
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

// RunLogin is `loomcycle login`.
func RunLogin(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runLogin(ctx, args, stdout, stderr, defaultLoginDeps())
}

// RunLogout is `loomcycle logout`.
func RunLogout(args []string, stdout, stderr io.Writer) int {
	return runLogout(context.Background(), args, stdout, stderr, defaultLoginDeps())
}

// RunAuth is `loomcycle auth <status>`.
func RunAuth(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "status" {
		return fail(stderr, "usage: loomcycle auth status [--url <server>]")
	}
	return runAuthStatus(context.Background(), args[1:], stdout, stderr, defaultLoginDeps())
}

func loginFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	u := fs.String("url", defaultBaseURL(), "loomcycle server URL (default: $LOOMCYCLE_BASE_URL or http://127.0.0.1:8787)")
	return fs, u
}

// deviceAuthorization is POST /v1/_auth/device's answer (RFC 8628 §3.2).
type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// deviceToken is POST /v1/_auth/device/token's answer: a token, or an RFC 6749
// error.
type deviceToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

func runLogin(ctx context.Context, args []string, stdout, stderr io.Writer, d loginDeps) int {
	fs, rawURL := loginFlags("login", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	server, err := canonicalServer(*rawURL)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	// Find the store first: a person who cannot keep the token should learn
	// it before approving a login whose token would be thrown away.
	st, err := d.store()
	if err != nil {
		return failOp(stderr, "%v", err)
	}
	form := url.Values{"client_id": {deviceClientID}}
	var da deviceAuthorization
	status, body, err := postForm(ctx, d.client, server+"/v1/_auth/device", form, &da)
	if err != nil {
		return failOp(stderr, "starting the login at %s: %v", server, err)
	}
	if status != http.StatusOK {
		return failOp(stderr, "the server refused to start a device login (HTTP %d): %s", status, serverText(body, 200))
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return failOp(stderr, "the server's device-login answer is incomplete")
	}
	// Both are printed to the terminal: refuse control characters a hostile
	// server could use to rewrite what the person sees.
	if vu, err := url.Parse(da.VerificationURI); err != nil || (vu.Scheme != "https" && vu.Scheme != "http") ||
		!printableASCII(da.VerificationURI) || !printableASCII(da.UserCode) || len(da.UserCode) > 32 {
		return failOp(stderr, "the server's device-login answer is malformed")
	}
	interval := time.Duration(da.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	expires := time.Duration(da.ExpiresIn) * time.Second
	if expires <= 0 || expires > time.Hour {
		expires = 10 * time.Minute
	}
	fmt.Fprintf(stderr, "To sign in to %s, open\n\n    %s\n\nand enter the code\n\n    %s\n\n", server, da.VerificationURI, da.UserCode)
	fmt.Fprintf(stderr, "Waiting for approval (the code expires in %s; Ctrl-C to stop)...\n", expires.Round(time.Second))

	ctx, cancel := context.WithTimeout(ctx, expires+30*time.Second)
	defer cancel()
	tokenForm := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
		"client_id":   {deviceClientID},
	}
	for {
		if err := d.sleep(ctx, interval); err != nil {
			return failOp(stderr, "login not completed: %v", err)
		}
		var tok deviceToken
		status, body, err := postForm(ctx, d.client, server+"/v1/_auth/device/token", tokenForm, &tok)
		if err != nil {
			return failOp(stderr, "polling %s: %v", server, err)
		}
		switch {
		case status == http.StatusOK && tok.AccessToken != "":
			if err := st.Set(ctx, server, tok.AccessToken); err != nil {
				return failOp(stderr, "signed in, but the token could not be stored: %v (it was not saved anywhere else)", err)
			}
			fmt.Fprintf(stdout, "Signed in to %s. The token is in %s", server, st.Name())
			if tok.ExpiresIn > 0 {
				fmt.Fprintf(stdout, " and expires in %s", (time.Duration(tok.ExpiresIn) * time.Second).Round(time.Minute))
			}
			fmt.Fprintln(stdout, ".")
			return 0
		case tok.Error == "authorization_pending":
		case tok.Error == "slow_down":
			// RFC 8628 §3.5: this and every later poll waits 5 s longer.
			interval += 5 * time.Second
		case tok.Error == "access_denied":
			return failOp(stderr, "the login was denied in the browser, or its approval no longer holds (the approving session ended); run `loomcycle login` again")
		case tok.Error == "expired_token":
			return failOp(stderr, "the code expired before it was approved; run `loomcycle login` again")
		default:
			return failOp(stderr, "the server refused the login (HTTP %d): %s", status, serverText(body, 200))
		}
	}
}

func runLogout(ctx context.Context, args []string, stdout, stderr io.Writer, d loginDeps) int {
	fs, rawURL := loginFlags("logout", stderr)
	forget := fs.Bool("forget", false, "remove the stored token even when the server cannot be reached to retire it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	server, err := canonicalServer(*rawURL)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	st, err := d.store()
	if err != nil {
		return failOp(stderr, "%v", err)
	}
	tok, err := st.Get(ctx, server)
	if errors.Is(err, errNoStoredToken) {
		fmt.Fprintf(stdout, "Not signed in to %s.\n", server)
		return 0
	}
	if err != nil {
		return failOp(stderr, "%v", err)
	}
	status, body, err := doBearer(ctx, d.client, http.MethodPost, server+"/v1/_auth/token/revoke", tok)
	switch {
	case err == nil && (status == http.StatusOK || status == http.StatusUnauthorized):
		// 401: the token no longer authenticates (expired, retired), so there
		// is nothing left to retire.
	case *forget:
		fmt.Fprintf(stderr, "warning: the token could not be retired on %s; it stays valid there until it expires\n", server)
	case err != nil:
		return failOp(stderr, "retiring the token on %s: %v (it is still stored; --forget removes it anyway)", server, err)
	default:
		return failOp(stderr, "the server did not retire the token (HTTP %d): %s (it is still stored; --forget removes it anyway)",
			status, serverText(body, 200))
	}
	if err := st.Delete(ctx, server); err != nil {
		return failOp(stderr, "%v", err)
	}
	fmt.Fprintf(stdout, "Signed out of %s.\n", server)
	return 0
}

func runAuthStatus(ctx context.Context, args []string, stdout, stderr io.Writer, d loginDeps) int {
	fs, rawURL := loginFlags("auth status", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	server, err := canonicalServer(*rawURL)
	if err != nil {
		return fail(stderr, "%v", err)
	}
	st, err := d.store()
	if err != nil {
		return failOp(stderr, "%v", err)
	}
	tok, err := st.Get(ctx, server)
	if errors.Is(err, errNoStoredToken) {
		fmt.Fprintf(stdout, "Not signed in to %s (no token in %s). Run `loomcycle login --url %s`.\n", server, st.Name(), server)
		return 1
	}
	if err != nil {
		return failOp(stderr, "%v", err)
	}
	status, body, err := doBearer(ctx, d.client, http.MethodGet, server+"/v1/_me", tok)
	if err != nil {
		return failOp(stderr, "a token is stored for %s, but the server could not be reached: %v", server, err)
	}
	if status == http.StatusUnauthorized {
		fmt.Fprintf(stdout, "The token stored for %s no longer works. Run `loomcycle login --url %s`.\n", server, server)
		return 1
	}
	if status != http.StatusOK {
		return failOp(stderr, "GET /v1/_me answered HTTP %d: %s", status, serverText(body, 200))
	}
	var me struct {
		TenantID string   `json:"tenant_id"`
		Subject  string   `json:"subject"`
		Scopes   []string `json:"scopes"`
		Auth     struct {
			Mode string `json:"mode"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		return failOp(stderr, "decoding /v1/_me: %v", err)
	}
	fmt.Fprintf(stdout, "Signed in to %s as %s (tenant %q), scopes %s; token in %s; server auth mode %s.\n",
		server, me.Subject, me.TenantID, strings.Join(me.Scopes, " "), st.Name(), me.Auth.Mode)
	return 0
}

// postForm POSTs an urlencoded form and decodes a JSON answer into out (best
// effort: an undecodable body leaves out zero and is returned raw).
func postForm(ctx context.Context, c *http.Client, u string, form url.Values, out any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	_ = json.Unmarshal(body, out)
	return resp.StatusCode, body, nil
}

func doBearer(ctx context.Context, c *http.Client, method, u, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, body, err
}

// StoredUpstreamToken is the token `loomcycle login` stored for server, for
// the MCP thin client. ok is false (with why, for a log line) when there is
// none or no keychain to read. It never starts a login: a stdio MCP server
// has no terminal to show a code on.
func StoredUpstreamToken(ctx context.Context, server string) (token string, why string, ok bool) {
	return storedUpstreamToken(ctx, server, osTokenStore)
}

func storedUpstreamToken(ctx context.Context, server string, open func() (tokenStore, error)) (string, string, bool) {
	canon, err := canonicalServer(server)
	if err != nil {
		return "", err.Error(), false
	}
	st, err := open()
	if err != nil {
		return "", err.Error(), false
	}
	tok, err := st.Get(ctx, canon)
	if errors.Is(err, errNoStoredToken) {
		return "", "no token stored for " + canon + "; run `loomcycle login --url " + canon + "`", false
	}
	if err != nil {
		return "", err.Error(), false
	}
	return tok, "", true
}

// serverText is a server's response body as it may be quoted to the
// terminal: capped at max bytes, every byte printableASCII would refuse
// replaced by '?', so a hostile server cannot rewrite what the person sees.
func serverText(b []byte, max int) string {
	s := []byte(truncate(b, max))
	for i, c := range s {
		if c < 0x20 || c > 0x7e {
			s[i] = '?'
		}
	}
	return string(s)
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
