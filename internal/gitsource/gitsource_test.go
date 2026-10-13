package gitsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

	f := &fixture{old: old, head: head, requests: new(atomic.Int64), sawAuth: new(atomic.Value)}
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
		MaxFetchBytes: 16 << 20,
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
	// 3 files + 3 links.
	if res.Files != 6 || res.Bytes != int64(len("readme\n")+len("package a\n")+len("#!/bin/sh\necho hi\n")) {
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
		MaxFetchBytes: 1 << 20,
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
	good := Request{URL: "https://git.example/acme/app", Commit: strings.Repeat("a", 40), StoreDir: "/tmp/x", MaxFetchBytes: 1,
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
