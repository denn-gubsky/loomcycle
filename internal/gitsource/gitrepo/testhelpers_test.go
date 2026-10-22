package gitrepo

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/util"

	"github.com/denn-gubsky/loomcycle/internal/gitsource"
)

// The network tests talk to a real git server: git's own http-backend, run
// as a CGI program behind a TLS test server (as gitsource's tests do). The
// served repository is built with the git command line. A machine without
// git skips them, loudly; on CI a skip would read as a pass, so it fails.

func gitBackend(t *testing.T) string {
	t.Helper()
	missing := t.Skipf
	if os.Getenv("CI") != "" {
		missing = t.Fatalf
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		missing("SKIPPED, NOT PASSED: git is not installed (%v); the network tests need git's http-backend", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(p); err != nil {
		missing("SKIPPED, NOT PASSED: %s not found; the network tests need git's http-backend", p)
	}
	return p
}

// run runs git in dir, isolated from the machine's git config, and returns
// its trimmed output.
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

const (
	testUser = "x-access-token"
	// testToken is distinctive so a search for it cannot match anything else.
	testToken = "tok-ZQ7x9WvKp3-never-on-disk"
)

// server is a served repository that accepts pushes.
type server struct {
	url       string
	transport http.RoundTripper
	work      string // a working copy, to make commits the server then has
	bare      string // the served repository
	first     string // the first commit
	head      string // main's tip when served
}

// newServer serves a repository of two commits on main. With requireAuth
// every request needs testUser and testToken.
func newServer(t *testing.T, requireAuth bool) *server {
	t.Helper()
	backend := gitBackend(t)
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, work, "init", "-q", "-b", "main")
	writeOS(t, work, "README.md", "first\n")
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "first")
	first := run(t, work, "rev-parse", "HEAD")
	writeOS(t, work, "README.md", "readme\n")
	writeOS(t, work, "src/a.go", "package a\n")
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "second")
	head := run(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(base, "served", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, base, "clone", "-q", "--bare", work, bare)
	run(t, bare, "config", "http.receivepack", "true")
	run(t, work, "remote", "add", "served", bare)

	git := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(base, "served"),
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		},
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAuth {
			u, p, ok := r.BasicAuth()
			if !ok || u != testUser || p != testToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		git.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &server{url: srv.URL + "/acme/app.git", transport: srv.Client().Transport, work: work, bare: bare, first: first, head: head}
}

// network is a Network to the server with the given token.
func (s *server) network(t *testing.T, token string) Network {
	t.Helper()
	u, err := url.Parse(s.url)
	if err != nil {
		t.Fatal(err)
	}
	return Network{
		URL: s.url, Username: testUser, Token: token,
		Guard:          gitsource.Guard{Host: gitsource.HostKey(u), Transport: s.transport, MaxReadBytes: 64 << 20},
		MaxObjectBytes: 1 << 20,
		SpoolDir:       t.TempDir(),
	}
}

// commit makes a commit on the server's main and returns its hash.
func (s *server) commit(t *testing.T, name, content string) string {
	t.Helper()
	writeOS(t, s.work, name, content)
	run(t, s.work, "add", name)
	run(t, s.work, "commit", "-q", "-m", "add "+name)
	run(t, s.work, "push", "-q", "served", "HEAD:refs/heads/main")
	return run(t, s.work, "rev-parse", "HEAD")
}

func writeOS(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// write writes a file into a billy filesystem.
func write(t *testing.T, fs billy.Filesystem, rel, content string) {
	t.Helper()
	if err := util.WriteFile(fs, rel, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, fs billy.Filesystem, rel string) string {
	t.Helper()
	b, err := util.ReadFile(fs, rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func exists(fs billy.Filesystem, rel string) bool {
	_, err := fs.Lstat(rel)
	return err == nil
}

// clock hands out increasing commit times, so history order is defined.
type clock struct{ t time.Time }

func (c *clock) sig() Signature {
	c.t = c.t.Add(time.Minute)
	return Signature{Name: "Ann", Email: "ann@example.com", When: c.t}
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

// memRepo is a new repository in memory.
func memRepo(t *testing.T) (*Repo, billy.Filesystem, *clock) {
	t.Helper()
	fs := memfs.New()
	r, err := Init(fs, "")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r, fs, newClock()
}

// commitAll stages everything and commits it.
func commitAll(t *testing.T, r *Repo, c *clock, msg string) string {
	t.Helper()
	if err := r.Add([]string{"."}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s := c.sig()
	h, err := r.Commit(CommitOptions{Message: msg, Author: s, Committer: s})
	if err != nil {
		t.Fatalf("Commit %q: %v", msg, err)
	}
	return h
}

// statusMap is Status as path -> "XY".
func statusMap(t *testing.T, r *Repo) map[string]string {
	t.Helper()
	st, err := r.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	m := map[string]string{}
	for _, f := range st {
		m[f.Path] = f.Staging + f.Worktree
	}
	return m
}
