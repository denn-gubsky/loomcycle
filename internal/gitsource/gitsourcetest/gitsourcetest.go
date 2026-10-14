// Package gitsourcetest serves a real git repository to tests that fetch
// one: git's own http-backend, run as a CGI program behind a TLS test server,
// over a repository built with the git command line. That is the protocol
// and the server behaviour a forge speaks; a hand-written fake would only
// resemble it.
//
// It needs git on the machine. Without it a test is skipped, loudly, and on
// CI it fails: a skip there would read as a pass for the whole fetch path.
package gitsourcetest

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Options choose how the repository is served.
type Options struct {
	// User and Token, when Token is set, are required as HTTP basic auth on
	// every request.
	User, Token string
}

// Repo is a served repository.
type Repo struct {
	// URL is the repository's https URL; Host is its "host:port".
	URL, Host string
	// Transport trusts the test server's certificate.
	Transport http.RoundTripper
	// Old is the first commit (not the tip of any ref); Head is the tip of
	// refs/heads/main.
	Old, Head string
	// Requests counts the requests the server has seen.
	Requests *atomic.Int64

	work, bare string
}

// Git runs git in dir with a fixed identity and no user or system
// configuration, and returns its trimmed output.
func Git(t testing.TB, dir string, args ...string) string {
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

func backend(t testing.TB) string {
	t.Helper()
	missing := t.Skipf
	if os.Getenv("CI") != "" {
		missing = t.Fatalf
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		missing("SKIPPED, NOT PASSED: git is not installed (%v); this test needs git's http-backend", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(p); err != nil {
		missing("SKIPPED, NOT PASSED: %s not found; this test needs git's http-backend", p)
	}
	return p
}

// New builds and serves a repository at <server>/acme/app.git.
//
// Its head commit holds: README.md, src/a.go, bin/run.sh (executable), a
// 5000-byte big.bin, two links that stay inside the tree (docs/latest,
// src/alias.go), one that leaves it (escape), one that is absolute
// (absolute), and a submodule entry (vendor/lib).
func New(t testing.TB, opt Options) *Repo {
	t.Helper()
	cgiPath := backend(t)
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, work, "init", "-q", "-b", "main")
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
	link := func(target, name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "first\n", 0o644)
	Git(t, work, "add", ".")
	Git(t, work, "commit", "-q", "-m", "first")
	old := Git(t, work, "rev-parse", "HEAD")

	write("README.md", "readme\n", 0o644)
	write("src/a.go", "package a\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\necho hi\n", 0o755)
	write("big.bin", strings.Repeat("x", 5000), 0o644)
	link("../README.md", "docs/latest")
	link("a.go", "src/alias.go")
	link("../outside", "escape")
	link("/etc/passwd", "absolute")
	Git(t, work, "add", ".")
	Git(t, work, "update-index", "--add", "--cacheinfo", "160000,"+old+",vendor/lib")
	Git(t, work, "commit", "-q", "-m", "second")
	head := Git(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(base, "served", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, base, "clone", "-q", "--bare", work, bare)
	Git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")

	r := &Repo{Old: old, Head: head, Requests: new(atomic.Int64), work: work, bare: bare}
	git := &cgi.Handler{
		Path: cgiPath,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(base, "served"),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.Requests.Add(1)
		if opt.Token != "" {
			u, p, ok := req.BasicAuth()
			if !ok || u != opt.User || p != opt.Token {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		git.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	r.URL = srv.URL + "/acme/app.git"
	if u, err := url.Parse(srv.URL); err == nil {
		r.Host = u.Host
	}
	r.Transport = srv.Client().Transport
	return r
}

// Commit adds one file to the repository as a new commit on main and
// returns its sha.
func (r *Repo) Commit(t testing.TB, name string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, r.work, "add", name)
	Git(t, r.work, "commit", "-q", "-m", "add "+name)
	Git(t, r.work, "push", "-q", r.bare, "HEAD:refs/heads/main")
	return Git(t, r.work, "rev-parse", "HEAD")
}
