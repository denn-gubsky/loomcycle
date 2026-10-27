package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testIssuedToken = "lct_3mJr7AoUXx2Wqd5KGtNn8YzPbVcR4s6E"

// fakeKeychainTool emulates /usr/bin/security and secret-tool well enough to
// drive the real store implementations, and records every invocation.
type fakeKeychainTool struct {
	mu    sync.Mutex
	items map[string]string // account -> secret
	calls []fakeCall
	// keepNothing makes a write report success without storing.
	keepNothing bool
}

type fakeCall struct {
	name  string
	args  []string
	stdin string
}

func newFakeKeychainTool() *fakeKeychainTool {
	return &fakeKeychainTool{items: map[string]string{}}
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// quoted extracts a single-quoted value after flag in a `security -i` line.
func quoted(line, flag string) string {
	i := strings.Index(line, flag+" '")
	if i < 0 {
		return ""
	}
	rest := line[i+len(flag)+2:]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func (f *fakeKeychainTool) run(_ context.Context, name string, args []string, stdin string) (runResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{name: name, args: append([]string(nil), args...), stdin: stdin})
	switch filepath.Base(name) {
	case "security":
		switch args[0] {
		case "-i":
			if !strings.HasPrefix(stdin, "add-generic-password ") {
				return runResult{exitCode: 1}, nil
			}
			if !f.keepNothing {
				f.items[quoted(stdin, "-a")] = quoted(stdin, "-w")
			}
			return runResult{}, nil
		case "find-generic-password":
			v, ok := f.items[argValue(args, "-a")]
			if !ok {
				return runResult{exitCode: macItemNotFound, stderr: "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."}, nil
			}
			return runResult{stdout: v + "\n"}, nil
		case "delete-generic-password":
			if _, ok := f.items[argValue(args, "-a")]; !ok {
				return runResult{exitCode: macItemNotFound}, nil
			}
			delete(f.items, argValue(args, "-a"))
			return runResult{}, nil
		}
	case "secret-tool":
		acct := argValue(args, "account")
		switch args[0] {
		case "store":
			if !f.keepNothing {
				f.items[acct] = stdin
			}
			return runResult{}, nil
		case "lookup":
			v, ok := f.items[acct]
			if !ok {
				return runResult{exitCode: 1}, nil
			}
			return runResult{stdout: v}, nil
		case "clear":
			delete(f.items, acct)
			return runResult{}, nil
		}
	}
	return runResult{exitCode: 2, stderr: "unexpected command"}, nil
}

func lookPathOK(name string) (string, error) { return "/usr/bin/" + name, nil }

func storesUnderTest(t *testing.T) map[string]*fakeKeychainTool {
	t.Helper()
	return map[string]*fakeKeychainTool{"darwin": newFakeKeychainTool(), "linux": newFakeKeychainTool()}
}

// The security property: whichever store, the token reaches the tool only on
// stdin — never in an argument, where `ps` shows it to every local user.
func TestTokenStore_PassesTheTokenOnlyOnStdin(t *testing.T) {
	for goos, tool := range storesUnderTest(t) {
		st, err := newOSTokenStore(goos, lookPathOK, tool.run)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := st.Set(ctx, "https://lc.example", testIssuedToken); err != nil {
			t.Fatalf("%s set: %v", goos, err)
		}
		got, err := st.Get(ctx, "https://lc.example")
		if err != nil || got != testIssuedToken {
			t.Fatalf("%s get = %q, %v", goos, got, err)
		}
		if err := st.Delete(ctx, "https://lc.example"); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Get(ctx, "https://lc.example"); !errors.Is(err, errNoStoredToken) {
			t.Fatalf("%s after delete: %v", goos, err)
		}
		if err := st.Delete(ctx, "https://lc.example"); err != nil {
			t.Fatalf("%s deleting nothing: %v", goos, err)
		}
		sawStdin := false
		for _, c := range tool.calls {
			for _, a := range c.args {
				if strings.Contains(a, testIssuedToken) {
					t.Fatalf("%s: the token appeared in argv of %s %v", goos, c.name, c.args)
				}
			}
			if strings.Contains(c.stdin, testIssuedToken) {
				sawStdin = true
			}
		}
		if !sawStdin {
			t.Fatalf("%s: the token never reached the tool", goos)
		}
	}
}

func TestTokenStore_RefusesATokenThatCouldBreakTheCommandLine(t *testing.T) {
	for goos, tool := range storesUnderTest(t) {
		st, _ := newOSTokenStore(goos, lookPathOK, tool.run)
		for _, bad := range []string{"lct_abc' -w 'x", "short", "lct_with space in it 123", "lct_newline\nadd-generic-password"} {
			if err := st.Set(context.Background(), "https://lc.example", bad); err == nil {
				t.Fatalf("%s accepted %q", goos, bad)
			}
		}
		if len(tool.calls) != 0 {
			t.Fatalf("%s ran the tool for a refused token: %+v", goos, tool.calls)
		}
	}
}

// A tool that reports success but keeps nothing must not leave login saying
// the person is signed in.
func TestTokenStore_SetFailsWhenTheTokenDoesNotReadBack(t *testing.T) {
	for goos, tool := range storesUnderTest(t) {
		tool.keepNothing = true
		st, _ := newOSTokenStore(goos, lookPathOK, tool.run)
		if err := st.Set(context.Background(), "https://lc.example", testIssuedToken); err == nil {
			t.Fatalf("%s: a write that kept nothing reported success", goos)
		}
	}
}

func TestOSTokenStore_RefusesPlatformsWithoutAKeychainInsteadOfUsingAFile(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, tc := range []struct{ goos string }{{"windows"}, {"plan9"}, {"darwin"}, {"linux"}} {
		_, err := newOSTokenStore(tc.goos, missing, newFakeKeychainTool().run)
		if !errors.Is(err, errNoKeychain) {
			t.Fatalf("%s with no tool: err = %v, want errNoKeychain", tc.goos, err)
		}
		if !strings.Contains(err.Error(), "LOOMCYCLE_MCP_UPSTREAM_TOKEN") && tc.goos != "darwin" {
			t.Fatalf("%s: the error does not say what to do instead: %v", tc.goos, err)
		}
	}
}

func TestCanonicalServer_NormalisesAndRefusesJunk(t *testing.T) {
	for in, want := range map[string]string{
		"https://LC.Example/":         "https://lc.example",
		"http://127.0.0.1:8787":       "http://127.0.0.1:8787",
		"https://lc.example/prefix/":  "https://lc.example/prefix",
		" https://lc.example:8443 ":   "https://lc.example:8443",
		"HTTPS://lc.example/a/b/../c": "https://lc.example/a/b/../c",
	} {
		got, err := canonicalServer(in)
		if err != nil || got != want {
			t.Fatalf("canonicalServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "lc.example", "ftp://lc.example", "https://u:p@lc.example", "https://lc.example/?x=1", "https://lc.example/#f", "https://lc.example/a'b"} {
		if got, err := canonicalServer(bad); err == nil {
			t.Fatalf("canonicalServer(%q) = %q, accepted", bad, got)
		}
	}
}

// deviceServer is a minimal RFC 8628 server: pending, then slow_down, then a
// token; and a revoke endpoint.
type deviceServer struct {
	mu       sync.Mutex
	polls    int
	revoked  []string
	approved bool
	deny     bool
}

func (d *deviceServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/_auth/device", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("client_id") != deviceClientID {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"lcd_dev","user_code":"BCDF-GHJK","verification_uri":"https://lc.example/ui/device",` +
			`"verification_uri_complete":"https://lc.example/ui/device#BCDF-GHJK","expires_in":600,"interval":5}`))
	})
	mux.HandleFunc("POST /v1/_auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("device_code") != "lcd_dev" || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("token request form = %v", r.PostForm)
		}
		d.mu.Lock()
		d.polls++
		n := d.polls
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case d.deny:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		case n == 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
		case n == 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"slow_down"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"` + testIssuedToken + `","token_type":"Bearer","expires_in":43200}`))
		}
	})
	mux.HandleFunc("POST /v1/_auth/token/revoke", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.revoked = append(d.revoked, r.Header.Get("Authorization"))
		d.mu.Unlock()
		_, _ = w.Write([]byte(`{"retired":true}`))
	})
	mux.HandleFunc("GET /v1/_me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testIssuedToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tenant_id":"acme","subject":"local:abc","scopes":["runs:create"],"auth":{"mode":"strict"}}`))
	})
	return mux
}

// isolateHome points HOME, XDG dirs and the working directory at empty temp
// dirs, so a file the CLI wrote anywhere a dotfile could go is found.
func isolateHome(t *testing.T) []string {
	t.Helper()
	home, xdgc, xdgd, cwd := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdgc)
	t.Setenv("XDG_DATA_HOME", xdgd)
	t.Setenv("XDG_STATE_HOME", xdgd)
	t.Chdir(cwd)
	return []string{home, xdgc, xdgd, cwd, os.TempDir()}
}

func assertTokenInNoFile(t *testing.T, dirs []string) {
	t.Helper()
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil || info.Size() > 1<<20 {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr == nil && bytes.Contains(b, []byte(testIssuedToken)) {
				t.Errorf("the token was written to %s", path)
			}
			return nil
		})
	}
}

func testDeps(t *testing.T, tool *fakeKeychainTool, goos string, sleeps *[]time.Duration) loginDeps {
	return loginDeps{
		store:  func() (tokenStore, error) { return newOSTokenStore(goos, lookPathOK, tool.run) },
		client: &http.Client{Timeout: 5 * time.Second},
		sleep: func(_ context.Context, d time.Duration) error {
			*sleeps = append(*sleeps, d)
			return nil
		},
	}
}

// End to end against a device server: the CLI honours slow_down, stores the
// token in the keychain only — no file anywhere a dotfile could go — and never
// prints it.
func TestLogin_StoresTheIssuedTokenOnlyInTheKeychain(t *testing.T) {
	dirs := isolateHome(t)
	ds := &deviceServer{}
	srv := httptest.NewServer(ds.handler(t))
	defer srv.Close()
	tool := newFakeKeychainTool()
	var sleeps []time.Duration
	var out, errb bytes.Buffer
	rc := runLogin(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, tool, "darwin", &sleeps))
	if rc != 0 {
		t.Fatalf("login rc=%d stderr=%s", rc, errb.String())
	}
	server, _ := canonicalServer(srv.URL)
	if tool.items[server] != testIssuedToken {
		t.Fatalf("keychain = %v", tool.items)
	}
	if strings.Contains(out.String()+errb.String(), testIssuedToken) {
		t.Fatal("the token was printed")
	}
	if !strings.Contains(errb.String(), "BCDF-GHJK") || !strings.Contains(errb.String(), "https://lc.example/ui/device") {
		t.Fatalf("the code and link were not shown: %s", errb.String())
	}
	// pending at 5s, slow_down at 5s, then 10s.
	if len(sleeps) != 3 || sleeps[0] != 5*time.Second || sleeps[1] != 5*time.Second || sleeps[2] != 10*time.Second {
		t.Fatalf("poll waits = %v, want [5s 5s 10s]", sleeps)
	}
	assertTokenInNoFile(t, dirs)

	// status reads it back through the server.
	out.Reset()
	if rc := runAuthStatus(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, tool, "darwin", &sleeps)); rc != 0 ||
		!strings.Contains(out.String(), "local:abc") || strings.Contains(out.String(), testIssuedToken) {
		t.Fatalf("status rc=%d out=%s", rc, out.String())
	}
	// logout retires it on the server, then removes it.
	out.Reset()
	if rc := runLogout(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, tool, "darwin", &sleeps)); rc != 0 {
		t.Fatalf("logout rc=%d stderr=%s", rc, errb.String())
	}
	if len(ds.revoked) != 1 || ds.revoked[0] != "Bearer "+testIssuedToken {
		t.Fatalf("revoke calls = %v", ds.revoked)
	}
	if _, ok := tool.items[server]; ok {
		t.Fatal("logout left the token in the keychain")
	}
	if rc := runAuthStatus(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, tool, "darwin", &sleeps)); rc != 1 {
		t.Fatalf("status after logout rc=%d", rc)
	}
}

func TestLogin_DeniedInTheBrowserStoresNothing(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer((&deviceServer{deny: true}).handler(t))
	defer srv.Close()
	tool := newFakeKeychainTool()
	var sleeps []time.Duration
	var out, errb bytes.Buffer
	if rc := runLogin(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, tool, "linux", &sleeps)); rc == 0 {
		t.Fatal("a denied login succeeded")
	}
	if len(tool.items) != 0 || !strings.Contains(errb.String(), "denied") {
		t.Fatalf("items=%v stderr=%s", tool.items, errb.String())
	}
}

// With no keychain the CLI refuses up front — before the person approves a
// login whose token would have nowhere to go — and never falls back to a file.
func TestLogin_WithoutAKeychainRefusesBeforeStartingTheFlow(t *testing.T) {
	dirs := isolateHome(t)
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	d := loginDeps{
		store: func() (tokenStore, error) {
			return newOSTokenStore("windows", lookPathOK, newFakeKeychainTool().run)
		},
		client: srv.Client(),
		sleep:  func(context.Context, time.Duration) error { return nil },
	}
	var out, errb bytes.Buffer
	if rc := runLogin(context.Background(), []string{"--url", srv.URL}, &out, &errb, d); rc == 0 {
		t.Fatal("login succeeded with no keychain")
	}
	if hit {
		t.Fatal("the device flow started although the token could not be kept")
	}
	if !strings.Contains(errb.String(), "keychain") {
		t.Fatalf("stderr = %s", errb.String())
	}
	assertTokenInNoFile(t, dirs)
}

func TestLogout_KeepsTheTokenWhenTheServerCannotRetireIt(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	tool := newFakeKeychainTool()
	server, _ := canonicalServer(srv.URL)
	tool.items[server] = testIssuedToken
	var sleeps []time.Duration
	var out, errb bytes.Buffer
	if rc := runLogout(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, tool, "darwin", &sleeps)); rc == 0 {
		t.Fatal("logout reported success though the token is still valid on the server")
	}
	if tool.items[server] != testIssuedToken {
		t.Fatal("the token was dropped without being retired")
	}
	if rc := runLogout(context.Background(), []string{"--url", srv.URL, "--forget"}, &out, &errb, testDeps(t, tool, "darwin", &sleeps)); rc != 0 {
		t.Fatalf("--forget rc=%d", rc)
	}
	if _, ok := tool.items[server]; ok {
		t.Fatal("--forget left the token")
	}
}

func TestLogin_RefusesAVerificationLinkWithControlCharacters(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"lcd_x","user_code":"BCDF\u001b[2JGHJK","verification_uri":"https://lc.example/ui/device","expires_in":600,"interval":5}`))
	}))
	defer srv.Close()
	var sleeps []time.Duration
	var out, errb bytes.Buffer
	if rc := runLogin(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, newFakeKeychainTool(), "darwin", &sleeps)); rc == 0 {
		t.Fatal("accepted a user code with an escape sequence")
	}
	if strings.Contains(errb.String(), "\x1b") {
		t.Fatal("printed the escape sequence")
	}
}

// A refusal's body is quoted to the terminal, so a hostile server's escape
// sequences must not reach it — the same filter as the link and user code.
func TestLogin_RefusalBodyIsPrintedWithoutControlCharacters(t *testing.T) {
	isolateHome(t)
	hostile := "denied\x1b]0;pwned\x07\x1b[2J\r\n" + strings.Repeat("x", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(hostile))
	}))
	defer srv.Close()
	var sleeps []time.Duration
	var out, errb bytes.Buffer
	if rc := runLogin(context.Background(), []string{"--url", srv.URL}, &out, &errb, testDeps(t, newFakeKeychainTool(), "darwin", &sleeps)); rc == 0 {
		t.Fatal("a refused start reported success")
	}
	msg := strings.TrimRight(errb.String(), "\n")
	if !printableASCII(msg) {
		t.Fatalf("printed control characters: %q", msg)
	}
	if !strings.Contains(msg, "denied") || strings.Contains(msg, strings.Repeat("x", 201)) {
		t.Fatalf("want the body's start, capped at 200 bytes: %q", msg)
	}
}

func TestStoredUpstreamToken_ReadsTheLoginTokenAndNeverStartsAFlow(t *testing.T) {
	tool := newFakeKeychainTool()
	tool.items["https://lc.example"] = testIssuedToken
	open := func() (tokenStore, error) { return newOSTokenStore("darwin", lookPathOK, tool.run) }
	tok, _, ok := storedUpstreamToken(context.Background(), "https://LC.example/", open)
	if !ok || tok != testIssuedToken {
		t.Fatalf("got %q, %v", tok, ok)
	}
	_, why, ok := storedUpstreamToken(context.Background(), "https://other.example", open)
	if ok || !strings.Contains(why, "loomcycle login") {
		t.Fatalf("missing token: ok=%v why=%q", ok, why)
	}
}
