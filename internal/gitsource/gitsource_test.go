package gitsource

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// These tests fetch from a real git server: git's own http-backend, run as a
// CGI program behind a TLS test server. That is the protocol and the server
// behaviour a forge speaks, which a hand-written fake would only resemble.
// The fixture repository is built with the git command line. Both need git
// on the machine; a machine without it skips them, loudly.

func gitBackend(t *testing.T) string {
	t.Helper()
	// On CI a skip would read as a pass for the whole fetch path, so there
	// the missing tool is a failure.
	missing := t.Skipf
	if os.Getenv("CI") != "" {
		missing = t.Fatalf
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		missing("SKIPPED, NOT PASSED: git is not installed (%v); the fetch tests need git's http-backend", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(p); err != nil {
		missing("SKIPPED, NOT PASSED: %s not found; the fetch tests need git's http-backend", p)
	}
	return p
}

// run runs git in dir and returns its trimmed output.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixture is a served repository and the commits of interest in it.
type fixture struct {
	url       string // https URL of the repository
	transport http.RoundTripper
	old       string // the first commit: not the tip of any ref
	head      string // the tip of refs/heads/main
	requests  *atomic.Int64
	work      string        // the working copy the served repository was cloned from
	bare      string        // the served repository
	sawAuth   *atomic.Value // the last Authorization header the server saw
}

const (
	goodUser  = "x-access-token"
	goodToken = "the-test-token"
)

// newFixture builds a repository, serves it, and returns the fixture. With
// requireAuth every request needs the good user and token. bySHA sets
// whether the server serves a commit that is not a ref tip by its sha.
func newFixture(t *testing.T, requireAuth, bySHA bool) *fixture {
	t.Helper()
	backend := gitBackend(t)
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, work, "init", "-q", "-b", "main")
	write := func(rel, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "first\n", 0o644)
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "first")
	old := run(t, work, "rev-parse", "HEAD")

	write("README.md", "readme\n", 0o644)
	write("src/a.go", "package a\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	write("big.bin", strings.Repeat("x", 5000), 0o644)
	link := func(target, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
			t.Fatal(err)
		}
	}
	link("../README.md", "docs/latest")        // stays inside: kept
	link("a.go", "src/alias.go")               // stays inside: kept
	link("../outside", "escape")               // leaves the tree
	link("/etc/passwd", "absolute")            // absolute
	link("src/../../etc/passwd", "sneaky")     // ".." after a name
	link("../../../etc/passwd", "docs/deeper") // more ".." than it is deep
	link("missing.txt", "dangling")            // inside, and leads nowhere
	run(t, work, "add", ".")
	// A submodule entry: another repository's commit at a path.
	run(t, work, "update-index", "--add", "--cacheinfo", "160000,"+old+",vendor/lib")
	run(t, work, "commit", "-q", "-m", "second")
	head := run(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(base, "served", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, base, "clone", "-q", "--bare", work, bare)
	if bySHA {
		run(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	} else {
		run(t, bare, "config", "uploadpack.allowAnySHA1InWant", "false")
		run(t, bare, "config", "uploadpack.allowReachableSHA1InWant", "false")
		run(t, bare, "config", "uploadpack.allowTipSHA1InWant", "false")
	}

	f := &fixture{old: old, head: head, work: work, bare: bare, requests: new(atomic.Int64), sawAuth: new(atomic.Value)}
	git := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(base, "served"),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.sawAuth.Store(r.Header.Get("Authorization"))
		if requireAuth {
			u, p, ok := r.BasicAuth()
			if !ok || u != goodUser || p != goodToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		git.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL + "/acme/app.git"
	f.transport = srv.Client().Transport
	return f
}

func (f *fixture) request(t *testing.T, commit string) Request {
	t.Helper()
	return Request{
		URL: f.url, Commit: commit, Transport: f.transport,
		StoreDir:      filepath.Join(t.TempDir(), "store"),
		MaxFetchBytes: 16 << 20, MaxObjectBytes: 1 << 20,
	}
}

var limits = TreeLimits{MaxBytes: 1 << 20, MaxFiles: 1000, MaxFileBytes: 1000, Reserved: []string{"_meta"}}

func TestFetchAndWriteTree_TheCommitAsFilesAndNothingElse(t *testing.T) {
	f := newFixture(t, false, true)
	req := f.request(t, f.head)
	got, err := Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer got.Close()
	if got.Head != f.head || got.FetchedBytes <= 0 {
		t.Errorf("Fetched = head %s, %d bytes", got.Head, got.FetchedBytes)
	}

	dest := t.TempDir()
	res, err := got.WriteTree(context.Background(), dest, limits)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}

	for rel, want := range map[string]string{"README.md": "readme\n", "src/a.go": "package a\n", "bin/run.sh": "#!/bin/sh\necho hi\n"} {
		b, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q (err %v), want %q", rel, b, err, want)
		}
	}
	// No file is executable, whatever the commit says.
	if fi, err := os.Stat(filepath.Join(dest, "bin/run.sh")); err != nil || fi.Mode().Perm()&0o111 != 0 {
		t.Errorf("bin/run.sh mode = %v (err %v), want no executable bit", fi.Mode().Perm(), err)
	}
	// Links that stay inside exist and lead where they say.
	if b, err := os.ReadFile(filepath.Join(dest, "docs/latest")); err != nil || string(b) != "readme\n" {
		t.Errorf("docs/latest -> %q (err %v), want the README", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "src/alias.go")); err != nil || string(b) != "package a\n" {
		t.Errorf("src/alias.go -> %q (err %v)", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "dangling")); err != nil {
		t.Errorf("a link to a missing file inside the tree should be kept: %v", err)
	}
	// Everything else is absent, and reported with its reason.
	reasons := map[string]string{}
	for _, o := range res.Omitted {
		reasons[o.Path] = o.Reason
	}
	want := map[string]string{
		"escape": ReasonSymlink, "absolute": ReasonSymlink, "sneaky": ReasonSymlink, "docs/deeper": ReasonSymlink,
		"vendor/lib": ReasonSubmodule, "big.bin": ReasonTooLarge,
	}
	for p, reason := range want {
		if reasons[p] != reason {
			t.Errorf("omitted[%s] = %q, want %q", p, reasons[p], reason)
		}
		if reason != ReasonSubmodule {
			if _, err := os.Lstat(filepath.Join(dest, p)); err == nil {
				t.Errorf("%s was written", p)
			}
		}
	}
	if len(res.Omitted) != len(want) || res.OmittedCount != len(want) {
		t.Errorf("omitted = %v (count %d), want exactly %d entries", res.Omitted, res.OmittedCount, len(want))
	}
	// The submodule is an empty directory.
	if entries, err := os.ReadDir(filepath.Join(dest, "vendor/lib")); err != nil || len(entries) != 0 {
		t.Errorf("vendor/lib: %v entries (err %v), want an empty directory", len(entries), err)
	}
	// 3 files + 3 links + the submodule's directory.
	if res.Files != 7 || res.Bytes != int64(len("readme\n")+len("package a\n")+len("#!/bin/sh\necho hi\n")) {
		t.Errorf("result = %d files, %d bytes", res.Files, res.Bytes)
	}
	// Nothing of git's is in the tree, and every link resolves inside it.
	realDest, _ := filepath.EvalSymlinks(dest)
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.EqualFold(d.Name(), ".git") {
			t.Errorf("the tree contains %s", p)
		}
		if d.Type()&os.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(p); err == nil && !strings.HasPrefix(resolved, realDest+string(filepath.Separator)) {
				t.Errorf("%s resolves outside the tree: %s", p, resolved)
			}
		}
		return nil
	})

	// Close removes the object store.
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(req.StoreDir); !os.IsNotExist(err) {
		t.Errorf("the object store survived Close: %v", err)
	}
}

// A commit that is not the tip of any ref: by sha where the server allows
// it, and through the ref where it does not.
func TestFetch_ACommitThatIsNotARefTip(t *testing.T) {
	t.Run("the server serves it by sha", func(t *testing.T) {
		f := newFixture(t, false, true)
		got, err := Fetch(context.Background(), f.request(t, f.old))
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		defer got.Close()
		dest := t.TempDir()
		if _, err := got.WriteTree(context.Background(), dest, limits); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dest, "README.md")); string(b) != "first\n" {
			t.Errorf("README.md = %q, want the first commit's", b)
		}
	})
	t.Run("the server refuses, and a ref is given", func(t *testing.T) {
		f := newFixture(t, false, false)
		req := f.request(t, f.old)
		req.Ref = "refs/heads/main"
		got, err := Fetch(context.Background(), req)
		if err != nil {
			t.Fatalf("Fetch through the ref: %v", err)
		}
		defer got.Close()
		dest := t.TempDir()
		if _, err := got.WriteTree(context.Background(), dest, limits); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(filepath.Join(dest, "README.md")); string(b) != "first\n" {
			t.Errorf("README.md = %q, want the first commit's, not the ref's tip", b)
		}
	})
	t.Run("the server refuses, and no ref is given", func(t *testing.T) {
		f := newFixture(t, false, false)
		req := f.request(t, f.old)
		if _, err := Fetch(context.Background(), req); !errors.Is(err, ErrCommitNotFound) {
			t.Errorf("err = %v, want commit_not_found", err)
		}
		if _, err := os.Stat(req.StoreDir); !os.IsNotExist(err) {
			t.Error("a failed fetch left its object store behind")
		}
	})
	t.Run("the server serves by sha and has no such commit", func(t *testing.T) {
		f := newFixture(t, false, true)
		if _, err := Fetch(context.Background(), f.request(t, strings.Repeat("ab", 20))); !errors.Is(err, ErrCommitNotFound) {
			t.Errorf("err = %v, want commit_not_found", err)
		}
	})
	t.Run("the commit is not within the history looked at", func(t *testing.T) {
		f := newFixture(t, false, false)
		req := f.request(t, strings.Repeat("ab", 20)) // a sha the repository does not have
		req.Ref = "refs/heads/main"
		if _, err := Fetch(context.Background(), req); !errors.Is(err, ErrCommitNotFound) {
			t.Errorf("err = %v, want commit_not_found", err)
		}
	})
}

func TestFetch_TokenAuthentication(t *testing.T) {
	f := newFixture(t, true, true)
	ok := f.request(t, f.head)
	ok.Username, ok.Token = goodUser, goodToken
	got, err := Fetch(context.Background(), ok)
	if err != nil {
		t.Fatalf("the right token: %v", err)
	}
	got.Close()

	for name, mut := range map[string]func(*Request){
		"a wrong token": func(r *Request) { r.Username, r.Token = goodUser, "wrong" },
		"no token":      func(*Request) {},
	} {
		req := f.request(t, f.head)
		mut(&req)
		_, err := Fetch(context.Background(), req)
		if !errors.Is(err, ErrAuthFailed) || Class(err) != "auth_failed" {
			t.Errorf("%s: err = %v (class %s), want auth_failed", name, err, Class(err))
		}
		if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), goodToken) {
			t.Errorf("%s: the error carries a token: %v", name, err)
		}
	}
}

// A failure that says nothing about the commit is not reported as the commit
// being missing, with or without a ref to fall back on.
func TestFetch_AServerThatCannotBeReachedIsNotACommitNotFound(t *testing.T) {
	f := newFixture(t, false, true)
	for _, ref := range []string{"", "refs/heads/main"} {
		req := f.request(t, f.head)
		req.URL = "https://127.0.0.1:1/acme/app.git" // nothing listens here
		req.Ref = ref
		_, err := Fetch(context.Background(), req)
		if err == nil || errors.Is(err, ErrCommitNotFound) || Class(err) != "fetch_failed" {
			t.Errorf("ref %q: err = %v (class %s), want fetch_failed", ref, err, Class(err))
		}
	}
}

func TestFetch_ARepositoryThatIsNotThere(t *testing.T) {
	f := newFixture(t, false, true)
	req := f.request(t, f.head)
	req.URL = strings.Replace(req.URL, "/acme/app.git", "/acme/nothing.git", 1)
	if _, err := Fetch(context.Background(), req); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v (class %s), want not_found", err, Class(err))
	}
}

// A 401, 403 or 404 body is the server's own text, read whole by the library
// into its error; it can be large, and can echo the request. The detail kept
// for the operator's log is the class, the status and a short snippet.
func TestFetch_ARefusalsBodyIsNotCarriedWhole(t *testing.T) {
	for status, class := range map[int]error{401: ErrAuthFailed, 403: ErrAuthFailed, 404: ErrNotFound} {
		body := "denied " + strings.Repeat("x", 64<<10) + " TAIL-OF-THE-BODY"
		req := Request{
			URL: "https://git.example/acme/app", Commit: strings.Repeat("a", 40),
			StoreDir: filepath.Join(t.TempDir(), "store"), MaxFetchBytes: 1 << 20, MaxObjectBytes: 1 << 20,
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}),
		}
		_, err := Fetch(context.Background(), req)
		if !errors.Is(err, class) {
			t.Errorf("%d: err class %s, want %v", status, Class(err), class)
			continue
		}
		if msg := err.Error(); len(msg) > 512 || strings.Contains(msg, "TAIL-OF-THE-BODY") {
			t.Errorf("%d: the error carries %d bytes of the body: %.200q…", status, len(msg), msg)
		} else if !strings.Contains(msg, fmt.Sprint(status)) || !strings.Contains(msg, "denied") {
			t.Errorf("%d: the error %q names neither the status nor the start of the body", status, msg)
		}
	}
}

func TestFetch_StopsAtTheByteLimitAndLeavesNothing(t *testing.T) {
	f := newFixture(t, false, true)
	req := f.request(t, f.head)
	req.MaxFetchBytes = 200 // the ref advertisement alone is larger
	_, err := Fetch(context.Background(), req)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v (class %s), want source_too_large", err, Class(err))
	}
	if _, err := os.Stat(req.StoreDir); !os.IsNotExist(err) {
		t.Error("a fetch stopped at the limit left its object store behind")
	}
	// The same fetch with room works: the limit is what stopped it.
	if got, err := Fetch(context.Background(), f.request(t, f.head)); err != nil {
		t.Errorf("with room: %v", err)
	} else {
		got.Close()
	}
}

func TestFetch_StopsAtTheDeadline(t *testing.T) {
	f := newFixture(t, false, true)
	slow := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(5 * time.Second):
			return f.transport.RoundTrip(r)
		}
	})
	req := f.request(t, f.head)
	req.Transport = slow
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Fetch(ctx, req)
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v (class %s), want timeout", err, Class(err))
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the fetch outlived its deadline by far: %s", d)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A redirect is not followed, so the token is never sent to where it points.
func TestFetch_ARedirectIsNotFollowedAndTheTokenStaysHome(t *testing.T) {
	var elsewhereHits atomic.Int64
	var elsewhereAuth atomic.Value
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHits.Add(1)
		elsewhereAuth.Store(r.Header.Get("Authorization"))
		http.Error(w, "not here either", http.StatusNotFound)
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
	}))
	defer redirecting.Close()

	req := Request{
		URL: redirecting.URL + "/acme/app.git", Commit: strings.Repeat("ab", 20),
		Username: goodUser, Token: goodToken,
		// One transport that trusts both test servers.
		Transport:     trustBoth(t, redirecting, elsewhere),
		StoreDir:      filepath.Join(t.TempDir(), "store"),
		MaxFetchBytes: 1 << 20, MaxObjectBytes: 1 << 20,
	}
	if _, err := Fetch(context.Background(), req); err == nil {
		t.Fatal("a fetch through a redirect succeeded")
	}
	if n := elsewhereHits.Load(); n != 0 {
		t.Errorf("the redirect was followed: %d request(s) reached the other server, Authorization %q", n, elsewhereAuth.Load())
	}
}

func trustBoth(t *testing.T, a, b *httptest.Server) http.RoundTripper {
	t.Helper()
	ta, tb := a.Client().Transport, b.Client().Transport
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if "https://"+r.URL.Host == b.URL {
			return tb.RoundTrip(r)
		}
		return ta.RoundTrip(r)
	})
}

// The dispatcher refuses any request that is not to the fetch's own host, and
// any request that belongs to no fetch at all.
func TestDispatcher_RefusesAnotherHostAndARequestOutsideAFetch(t *testing.T) {
	reached := false
	st := &call{host: "git.example", max: 1 << 20, rt: roundTripFunc(func(*http.Request) (*http.Response, error) {
		reached = true
		return nil, errors.New("stop here")
	})}
	for _, u := range []string{"https://evil.example/acme/app.git/info/refs", "http://git.example/acme/app.git/info/refs", "https://git.example.evil.example/x"} {
		r, _ := http.NewRequestWithContext(withCall(context.Background(), st), http.MethodGet, u, nil)
		if _, err := (dispatcher{}).RoundTrip(r); err == nil || reached {
			t.Errorf("%s: err=%v reached=%v, want it refused before any transport", u, err, reached)
		}
	}
	r, _ := http.NewRequest(http.MethodGet, "https://git.example/x", nil)
	if _, err := (dispatcher{}).RoundTrip(r); err == nil || reached {
		t.Error("a request with no fetch behind it was sent")
	}
	// The fetch's own host, in any letter case, does go through.
	r, _ = http.NewRequestWithContext(withCall(context.Background(), st), http.MethodGet, "https://GIT.example/x", nil)
	if _, err := (dispatcher{}).RoundTrip(r); !reached || err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Errorf("the fetch's own host did not reach its transport: reached=%v err=%v", reached, err)
	}
}

// A repository named with https's own port is fetched: the library drops
// ":443" from the URLs it requests, and the dispatcher compared the host as
// written, so every request was refused. Another port still has to match.
func TestDispatcher_AcceptsTheDefaultPortWrittenOrNot(t *testing.T) {
	for _, repo := range []string{
		"https://github.com:443/acme/app",
		"https://GitHub.com/acme/app",
		"https://[2001:db8::5]:443/acme/app",
		"https://git.example:8443/acme/app",
	} {
		req := Request{URL: repo, Commit: strings.Repeat("a", 40), StoreDir: "/tmp/x", MaxFetchBytes: 1, MaxObjectBytes: 1, Transport: roundTripFunc(nil)}
		host, err := req.validate()
		if err != nil {
			t.Fatalf("%s: %v", repo, err)
		}
		// The URL the library itself requests for this repository.
		ep, err := transport.NewEndpoint(repo)
		if err != nil {
			t.Fatal(err)
		}
		reached := false
		st := &call{host: host, max: 1 << 20, rt: roundTripFunc(func(*http.Request) (*http.Response, error) {
			reached = true
			return nil, errors.New("stop here")
		})}
		r, _ := http.NewRequestWithContext(withCall(context.Background(), st), http.MethodGet, ep.String()+"/info/refs", nil)
		if _, err := (dispatcher{}).RoundTrip(r); !reached {
			t.Errorf("%s: the library's request %s was refused: %v", repo, r.URL, err)
		}
		// The same host on another port is not the repository's.
		other := *r.URL
		other.Host = other.Hostname() + ":9999"
		if strings.Contains(other.Hostname(), ":") {
			other.Host = "[" + other.Hostname() + "]:9999"
		}
		reached = false
		r2, _ := http.NewRequestWithContext(withCall(context.Background(), st), http.MethodGet, other.String(), nil)
		if _, err := (dispatcher{}).RoundTrip(r2); err == nil || reached {
			t.Errorf("%s: a request to %s went through", repo, other.Host)
		}
	}
}

func TestWriteTree_LimitsAreErrorsAndTheDestinationMustBeEmpty(t *testing.T) {
	f := newFixture(t, false, true)
	got, err := Fetch(context.Background(), f.request(t, f.head))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	for name, lim := range map[string]TreeLimits{
		"too many files": {MaxBytes: 1 << 20, MaxFiles: 2, MaxFileBytes: 1000},
		"too many bytes": {MaxBytes: 10, MaxFiles: 1000, MaxFileBytes: 1000},
	} {
		if _, err := got.WriteTree(context.Background(), t.TempDir(), lim); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: err = %v, want source_too_large", name, err)
		}
	}
	used := t.TempDir()
	if err := os.WriteFile(filepath.Join(used, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := got.WriteTree(context.Background(), used, limits); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a non-empty destination: err = %v, want invalid_request", err)
	}
	if _, err := got.WriteTree(context.Background(), t.TempDir(), TreeLimits{}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("no limits: err = %v, want invalid_request", err)
	}
	// A reserved top-level name is the caller's.
	work := t.TempDir()
	reservedAll := TreeLimits{MaxBytes: 1 << 20, MaxFiles: 1000, MaxFileBytes: 1000, Reserved: []string{"SRC"}}
	res, err := got.WriteTree(context.Background(), work, reservedAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, "src")); err == nil {
		t.Error("a path under a reserved name was written")
	}
	n := 0
	for _, o := range res.Omitted {
		if strings.HasPrefix(o.Path, "src/") && o.Reason == ReasonUnsafePath {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d paths under the reserved name were reported, want 2 (src/a.go, src/alias.go): %v", n, res.Omitted)
	}
}

func TestRequestValidate_RefusesWhatWouldMisleadBeforeAnyNetwork(t *testing.T) {
	good := Request{URL: "https://git.example/acme/app", Commit: strings.Repeat("a", 40), StoreDir: "/tmp/x", MaxFetchBytes: 1, MaxObjectBytes: 1,
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("a refused request reached the network")
			return nil, errors.New("no")
		})}
	if _, err := good.validate(); err != nil {
		t.Fatalf("a good request: %v", err)
	}
	for name, mut := range map[string]func(*Request){
		"http":               func(r *Request) { r.URL = "http://git.example/acme/app" },
		"ssh":                func(r *Request) { r.URL = "ssh://git@git.example/acme/app" },
		"a token in the URL": func(r *Request) { r.URL = "https://tok@git.example/acme/app" },
		"a query":            func(r *Request) { r.URL = "https://git.example/acme/app?x=1" },
		"an encoded path":    func(r *Request) { r.URL = "https://git.example/acme/%61pp" },
		"a backslash":        func(r *Request) { r.URL = "https://git.example\\@evil.example/acme/app" },
		"a space":            func(r *Request) { r.URL = "https://git.example/acme/app x" },
		"a newline":          func(r *Request) { r.URL = "https://git.example/acme/app\n" },
		"a sha-256":          func(r *Request) { r.Commit = strings.Repeat("a", 64) },
		"no object limit":    func(r *Request) { r.MaxObjectBytes = 0 },
		"a short sha":        func(r *Request) { r.Commit = "abc123" },
		"an upper-case sha":  func(r *Request) { r.Commit = strings.Repeat("A", 40) },
		"a branch name":      func(r *Request) { r.Commit = "main" },
		"a short ref":        func(r *Request) { r.Ref = "main" },
		"a ref with a colon": func(r *Request) { r.Ref = "refs/heads/a:refs/heads/b" },
		"a ref with ..":      func(r *Request) { r.Ref = "refs/heads/../x" },
		"no transport":       func(r *Request) { r.Transport = nil },
		"no limit":           func(r *Request) { r.MaxFetchBytes = 0 },
		"no store":           func(r *Request) { r.StoreDir = "" },
	} {
		req := good
		mut(&req)
		if _, err := Fetch(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want invalid_request", name, err)
		}
	}
}

func TestLinkStaysInside(t *testing.T) {
	for _, c := range []struct {
		link, target string
		want         bool
	}{
		{"l", "file", true},
		{"l", "dir/file", true},
		{"l", "./file", true},
		{"a/l", "../file", true},
		{"a/b/l", "../../file", true},
		{"a/b/l", "../x/y", true},
		{"l", "../file", false},                 // up from the top
		{"a/l", "../../file", false},            // more ups than depth
		{"a/l", "b/../file", false},             // ".." after a name
		{"a/l", "b/../../../etc/passwd", false}, // the same, leaving
		{"a/b/l", "../c/../../x", false},        // ".." after a name, however it nets out
		{"l", "/etc/passwd", false},
		{"l", "", false},
		{"l", "a\\b", false},
		{"l", "C:evil", false},
		{"l", ".git/config", false},
		{"a/l", "../.GIT/config", false},
		{"l", "a\x00b", false},
	} {
		if got := linkStaysInside(c.link, c.target); got != c.want {
			t.Errorf("linkStaysInside(%q, %q) = %v, want %v", c.link, c.target, got, c.want)
		}
	}
}

func TestSafeRelPath(t *testing.T) {
	for p, want := range map[string]bool{
		"a": true, "a/b.go": true, "a/.gitignore": true, "a/.github/x": true, "a b/c": true, "git/x": true,
		"": false, "/abs": false, "a//b": false, "a/./b": false, "a/../b": false, "..": false,
		".git/config": false, "a/.git/config": false, "a/.GIT/x": false, ".git": false, ".git./config": false, ".git /config": false,
		"a\\b": false, "a:b": false, "a\nb": false, "a\x00b": false, "a/...": false,
		strings.Repeat("d/", maxPathDepth-1) + "f": true,
		strings.Repeat("d/", maxPathDepth) + "f":   false, // a directory per segment, for one counted file
		strings.Repeat("n", maxPathLength+1):       false,
	} {
		if got := safeRelPath(p); got != want {
			t.Errorf("safeRelPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestClass_IsOneOfTheClosedList(t *testing.T) {
	if got := Class(errors.New("anything else")); got != "fetch_failed" {
		t.Errorf("an unknown error is class %q, want fetch_failed", got)
	}
}

// commit adds one file to the served repository as a new commit on main and
// returns its sha.
func (f *fixture) commit(t *testing.T, name string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, f.work, "add", name)
	run(t, f.work, "commit", "-q", "-m", "add "+name)
	run(t, f.work, "push", "-q", f.bare, "HEAD:refs/heads/main")
	return run(t, f.work, "rev-parse", "HEAD")
}

// A pack is compressed, so a limit on the bytes fetched says nothing about
// what they become: 64 MiB of zeros travels as about 64 KiB. Before the pack
// was checked on arrival the library inflated such an object whole into
// memory (600 MiB for a 600 KiB fetch, measured). It must fail the fetch,
// and without the memory.
func TestFetch_AnObjectThatInflatesPastTheLimitFailsWithoutBeingInflated(t *testing.T) {
	f := newFixture(t, false, true)
	const bomb = 64 << 20
	head := f.commit(t, "zeros.bin", make([]byte, bomb))

	req := f.request(t, head) // MaxObjectBytes is 1 MiB, MaxFetchBytes 16 MiB
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Fetch(context.Background(), req)
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrTooLarge) || Class(err) != "source_too_large" {
		t.Fatalf("err = %v (class %s), want source_too_large", err, Class(err))
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > bomb/4 {
		t.Errorf("the fetch allocated %d MiB on the way to refusing a %d MiB object", grew>>20, bomb>>20)
	}
	if _, err := os.Stat(req.StoreDir); !os.IsNotExist(err) {
		t.Error("the refused fetch left its object store behind")
	}

	// The same commit with room for the object is fetched: it was the
	// object's size that was refused, and the fetch itself was small.
	roomy := f.request(t, head)
	roomy.MaxObjectBytes = 2 * bomb
	got, err := Fetch(context.Background(), roomy)
	if err != nil {
		t.Fatalf("with room for the object: %v", err)
	}
	defer got.Close()
	if got.FetchedBytes > 4<<20 {
		t.Errorf("the fetch read %d bytes; the test expects a small pack for a large object", got.FetchedBytes)
	}
}

// packOf builds a pack holding one object: its type, the length its header
// declares, and its (compressed) body.
func packOf(t *testing.T, objType byte, declared uint64, body []byte) []byte {
	t.Helper()
	return packOfN(t, 1, objType, declared, body)
}

// packOfN is packOf with n copies of the object.
func packOfN(t *testing.T, n uint32, objType byte, declared uint64, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("PACK")
	_ = binary.Write(&b, binary.BigEndian, uint32(2))
	_ = binary.Write(&b, binary.BigEndian, n)
	for i := uint32(0); i < n; i++ {
		// The object header: 3 bits of type and the length, 4 bits then 7 at a time.
		d := declared
		first := objType<<4 | byte(d&0x0f)
		d >>= 4
		for d > 0 {
			b.WriteByte(first | 0x80)
			first = byte(d & 0x7f)
			d >>= 7
		}
		b.WriteByte(first)
		if objType == 7 { // a delta against an object named by its sha
			b.Write(make([]byte, 20))
		}
		z := zlib.NewWriter(&b)
		_, _ = z.Write(body)
		_ = z.Close()
	}
	b.Write(make([]byte, 20)) // the pack's checksum, not read by the check
	return b.Bytes()
}

// unlimited is a total no test of one object's size comes near.
func unlimited() *int64 {
	n := int64(1) << 62
	return &n
}

func varint(n uint64) []byte {
	var out []byte
	for {
		c := byte(n & 0x7f)
		n >>= 7
		if n == 0 {
			return append(out, c)
		}
		out = append(out, c|0x80)
	}
}

// The two ways a few bytes name a huge object: a header that declares it,
// and a delta whose instructions build it.
func TestCheckPack_RefusesAnObjectDeclaredOrBuiltPastTheLimit(t *testing.T) {
	const limit = 1 << 20
	ctx := context.Background()
	small := packOf(t, 3, 5, []byte("hello")) // a blob of 5 bytes
	if err := checkPack(ctx, bytes.NewReader(small), limit, unlimited()); err != nil {
		t.Fatalf("a small object: %v", err)
	}
	declared := packOf(t, 3, 1<<40, []byte("hello")) // a blob that says it is a terabyte
	if err := checkPack(ctx, bytes.NewReader(declared), limit, unlimited()); !errors.Is(err, errObjectTooLarge) {
		t.Errorf("a header declaring a terabyte: %v, want the refusal", err)
	}
	// A delta of a dozen bytes whose own header says the result is a terabyte.
	instr := append(append(varint(10), varint(1<<40)...), 0x01, 'x')
	delta := packOf(t, 7, uint64(len(instr)), instr)
	if err := checkPack(ctx, bytes.NewReader(delta), limit, unlimited()); !errors.Is(err, errObjectTooLarge) {
		t.Errorf("a delta that builds a terabyte: %v, want the refusal", err)
	}
	// The same delta building something small passes.
	fine := append(append(varint(10), varint(100)...), 0x01, 'x')
	if err := checkPack(ctx, bytes.NewReader(packOf(t, 7, uint64(len(fine)), fine)), limit, unlimited()); err != nil {
		t.Errorf("a delta that builds 100 bytes: %v", err)
	}
	if err := checkPack(ctx, bytes.NewReader([]byte("not a pack")), limit, unlimited()); err == nil || errors.Is(err, errObjectTooLarge) {
		t.Errorf("garbage: %v, want an error that is not the size refusal", err)
	}
}

// Each object under the limit is not enough: a few bytes per delta can each
// build the largest object allowed, and a fetch's worth of them is millions.
// What the objects build in all is bounded too, across the packs of a fetch.
func TestCheckPack_RefusesObjectsThatTogetherBuildPastTheTotal(t *testing.T) {
	const limit = 1 << 20
	ctx := context.Background()
	instr := append(append(varint(10), varint(limit)...), 0x01, 'x') // each builds 1 MiB
	pack := packOfN(t, 4, 7, uint64(len(instr)), instr)

	room := int64(4 * limit)
	if err := checkPack(ctx, bytes.NewReader(pack), limit, &room); err != nil {
		t.Fatalf("four deltas of 1 MiB with room for four: %v", err)
	}
	if room != 0 {
		t.Errorf("room left = %d, want 0", room)
	}
	// The next pack of the same fetch has no room left.
	if err := checkPack(ctx, bytes.NewReader(packOf(t, 3, 5, []byte("hello"))), limit, &room); !errors.Is(err, errObjectTooLarge) {
		t.Errorf("an object past the total: %v, want the refusal", err)
	}
	short := int64(3 * limit)
	if err := checkPack(ctx, bytes.NewReader(pack), limit, &short); !errors.Is(err, errObjectTooLarge) {
		t.Errorf("four deltas of 1 MiB with room for three: %v, want the refusal", err)
	}
	// Blobs count as their headers declare.
	blobs := packOfN(t, 3, 3, 5, []byte("hello"))
	tight := int64(14)
	if err := checkPack(ctx, bytes.NewReader(blobs), limit, &tight); !errors.Is(err, errObjectTooLarge) {
		t.Errorf("three 5-byte blobs with room for 14 bytes: %v, want the refusal", err)
	}
}

// The check of a pack stops at the fetch's deadline, between objects.
func TestCheckPack_StopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := checkPack(ctx, bytes.NewReader(packOfN(t, 3, 3, 5, []byte("hello"))), 1<<20, unlimited())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the context's", err)
	}
}

// End to end: objects each under the object limit, that together build more
// than the fetch's total, fail the fetch and leave nothing.
func TestFetch_ObjectsThatTogetherInflatePastTheTotalFail(t *testing.T) {
	f := newFixture(t, false, true)
	const each = 3 << 20
	var head string
	for i, c := range []byte("abc") {
		head = f.commit(t, fmt.Sprintf("big%d.txt", i), bytes.Repeat([]byte{c}, each))
	}
	req := f.request(t, head)
	req.MaxObjectBytes = 4 << 20
	req.MaxFetchBytes = 256 << 10 // a total of 8 MiB: under the 9 MiB the three build
	_, err := Fetch(context.Background(), req)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v (class %s), want source_too_large", err, Class(err))
	}
	if _, err := os.Stat(req.StoreDir); !os.IsNotExist(err) {
		t.Error("the refused fetch left its object store behind")
	}
	// With room for them, the same commit is fetched: the pack was small.
	roomy := f.request(t, head)
	roomy.MaxObjectBytes = 4 << 20
	roomy.MaxFetchBytes = 1 << 20
	got, err := Fetch(context.Background(), roomy)
	if err != nil {
		t.Fatalf("with room: %v", err)
	}
	got.Close()
}

func TestDeltaResultSize(t *testing.T) {
	for _, want := range []uint64{0, 1, 127, 128, 300, 1 << 20, 1 << 40} {
		b := append(varint(12345), varint(want)...)
		if got, ok := deltaResultSize(b); !ok || uint64(got) != want {
			t.Errorf("deltaResultSize(...%d) = %d, %v", want, got, ok)
		}
	}
	for _, bad := range [][]byte{nil, {0x80}, {0x05}, {0x05, 0x80}} {
		if _, ok := deltaResultSize(bad); ok {
			t.Errorf("deltaResultSize(%v) read a size from a cut-off header", bad)
		}
	}
}

// The object store may not sit inside the tree it is written to.
func TestWriteTree_RefusesAStoreInsideTheDestination(t *testing.T) {
	f := newFixture(t, false, true)
	dest := t.TempDir()
	req := f.request(t, f.head)
	req.StoreDir = filepath.Join(dest, ".store")
	got, err := Fetch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if _, err := got.WriteTree(context.Background(), dest, limits); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want invalid_request", err)
	}
}

// On a file system that ignores case (macOS by default), a link "q/A" to ".."
// is the directory "q/a" too, so a link written as "q/a/l" lands at the root
// of the tree, two levels up from where its name says, and its target of
// "../../x" (two ups, allowed at that depth) leads out. It dangled, and the
// final check skipped dangling links, so it stayed: a later write to "l"
// would create a file outside the volume. The path is refused under a link
// whatever its case, and no link that leads out survives, dangling or not.
//
// Which names are one is the file system's to say: case folding alone would
// miss a pair that differs in Unicode normalization (macOS treats the two
// spellings of "é" as one name) or in a case rule of its own. So every pair
// here is checked against the file system the test runs on: where it holds
// the two names to be one, the path under the link must be refused; where
// it does not, the path is an ordinary one and is written.
func TestWriteTree_ALinkUnderALinkOfAnotherCaseIsRefused(t *testing.T) {
	f := newFixture(t, false, true)
	link := func(path, target string) {
		t.Helper()
		blob := filepath.Join(t.TempDir(), "blob")
		if err := os.WriteFile(blob, []byte(target), 0o644); err != nil {
			t.Fatal(err)
		}
		sha := run(t, f.work, "hash-object", "-w", blob)
		run(t, f.work, "-c", "core.ignorecase=false", "-c", "core.precomposeunicode=false",
			"update-index", "--add", "--cacheinfo", "120000,"+sha+","+path)
	}
	// sameName reports whether this file system takes a and b to be one name.
	sameName := func(a, b string) bool {
		scratch := t.TempDir()
		if err := os.Mkdir(filepath.Join(scratch, a), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := os.Lstat(filepath.Join(scratch, b))
		return err == nil
	}
	// Written as code points, so no editor or tool normalizes them. Each
	// link's name sorts before the other spelling, so a tree walk writes
	// the link first.
	var (
		eAcute  = string(rune(0x00e9))       // é as one code point
		eAccent = "e" + string(rune(0x0301)) // é as e and a combining accent
		bigE    = "E" + string(rune(0x0301)) // É as E and a combining accent
		kelvin  = string(rune(0x212a))       // the Kelvin sign
	)
	pairs := []struct{ dir, linkName, otherName string }{
		{"q", "A", "a"},        // case
		{"n", eAccent, eAcute}, // normalization
		{"k", "k", kelvin},     // a case rule beyond ASCII
		{"s", bigE, eAcute},    // case and normalization at once
	}
	for _, p := range pairs {
		link(p.dir+"/"+p.linkName, "..")
		link(p.dir+"/"+p.otherName+"/l-"+p.dir, "../../zz-nonexistent")
	}
	run(t, f.work, "commit", "-q", "-m", "links of two spellings")
	run(t, f.work, "push", "-q", f.bare, "HEAD:refs/heads/main")
	got, err := Fetch(context.Background(), f.request(t, run(t, f.work, "rev-parse", "HEAD")))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	dest := t.TempDir()
	res, err := got.WriteTree(context.Background(), dest, limits)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, o := range res.Omitted {
		reasons[o.Path] = o.Reason
	}
	for _, p := range pairs {
		under := p.dir + "/" + p.otherName + "/l-" + p.dir
		if sameName(p.linkName, p.otherName) {
			if reasons[under] != ReasonUnsafePath {
				t.Errorf("%q, under the link %q of another spelling: omitted as %q, want %q", under, p.dir+"/"+p.linkName, reasons[under], ReasonUnsafePath)
			}
		} else if reasons[under] != "" {
			t.Errorf("%q, under a directory this file system tells apart from the link: omitted as %q, want written", under, reasons[under])
		}
	}
	// Whatever was written, no link on disk leads out: each is judged by the
	// directory it really sits in, not by its name.
	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		t.Fatal(err)
	}
	_ = filepath.WalkDir(realDest, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.Type()&os.ModeSymlink == 0 {
			return err
		}
		target, rerr := os.Readlink(p)
		if rerr != nil {
			t.Errorf("readlink %s: %v", p, rerr)
			return nil
		}
		if lands := filepath.Join(filepath.Dir(p), target); !within(lands, realDest) {
			t.Errorf("%s -> %s leads out of the tree", p, target)
		}
		return nil
	})
}

// A link that dangles and leads out is removed: a write to its name would
// create the target. One that dangles inside (its target omitted) is kept.
func TestLinkLandsInside_JudgesADanglingLinkByWhereItSits(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"q/A":     "..",
		"l":       "../../zz-nonexistent",
		"missing": "not-there",
		"sub/up":  "../not-there",
		"abs":     "/tmp/zz-nonexistent",
	} {
		p := filepath.Join(realRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]bool{
		"q/A":     true,  // the root itself
		"missing": true,  // dangling, inside
		"sub/up":  true,  // dangling, inside
		"l":       false, // dangling, outside
		"q/A/l":   false, // the same link reached through q/A: where it sits decides
		"abs":     false,
	} {
		if got := linkLandsInside(realRoot, name); got != want {
			t.Errorf("linkLandsInside(%s) = %v, want %v", name, got, want)
		}
	}
}

// A submodule entry makes a directory, so it counts toward the file limit.
func TestWriteTree_SubmoduleDirectoriesCountTowardTheFileLimit(t *testing.T) {
	f := newFixture(t, false, true)
	for i := 0; i < 5; i++ {
		run(t, f.work, "update-index", "--add", "--cacheinfo", fmt.Sprintf("160000,%s,mods/m%d", f.old, i))
	}
	run(t, f.work, "commit", "-q", "-m", "more submodules")
	run(t, f.work, "push", "-q", f.bare, "HEAD:refs/heads/main")
	got, err := Fetch(context.Background(), f.request(t, run(t, f.work, "rev-parse", "HEAD")))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	// 3 files, 3 links and 6 submodule directories are 12; a limit of 10 is passed.
	tight := TreeLimits{MaxBytes: 1 << 20, MaxFiles: 10, MaxFileBytes: 1000}
	if _, err := got.WriteTree(context.Background(), t.TempDir(), tight); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want source_too_large", err)
	}
	if _, err := got.WriteTree(context.Background(), t.TempDir(), TreeLimits{MaxBytes: 1 << 20, MaxFiles: 12, MaxFileBytes: 1000}); err != nil {
		t.Errorf("at exactly the limit: %v", err)
	}
}
