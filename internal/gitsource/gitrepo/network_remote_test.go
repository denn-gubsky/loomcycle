package gitrepo

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-billy/v5/util"
	"github.com/go-git/go-git/v5/plumbing"
)

// twinServer serves two repositories on one host, app.git and other.git,
// and records every path requested.
type twinServer struct {
	*server
	otherURL  string
	otherBare string
	mu        sync.Mutex
	paths     []string
}

func newTwinServer(t *testing.T) *twinServer {
	t.Helper()
	backend := gitBackend(t)
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, work, "init", "-q", "-b", "main")
	writeOS(t, work, "README.md", "readme\n")
	run(t, work, "add", ".")
	run(t, work, "commit", "-q", "-m", "first")
	head := run(t, work, "rev-parse", "HEAD")
	root := filepath.Join(base, "served")
	bare := filepath.Join(root, "acme", "app.git")
	other := filepath.Join(root, "acme", "other.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{bare, other} {
		run(t, base, "clone", "-q", "--bare", work, b)
		run(t, b, "config", "http.receivepack", "true")
	}
	run(t, work, "remote", "add", "served", bare)

	ts := &twinServer{}
	git := &cgi.Handler{Path: backend, Env: []string{
		"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.paths = append(ts.paths, r.URL.Path)
		ts.mu.Unlock()
		git.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ts.server = &server{url: srv.URL + "/acme/app.git", transport: srv.Client().Transport, work: work, bare: bare, first: head, head: head}
	ts.otherURL, ts.otherBare = srv.URL+"/acme/other.git", other
	return ts
}

// touchedOther reports whether any request since the last call went to
// other.git, and forgets them.
func (ts *twinServer) touchedOther() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []string
	for _, p := range ts.paths {
		if strings.Contains(p, "other.git") {
			out = append(out, p)
		}
	}
	ts.paths = nil
	return out
}

// The URL a caller validated is the one dialled, whatever the repository's
// config says by the time the operation runs: here the remote is edited,
// after RemoteURL was read, to list other.git, app.git, other.git. The
// library fetches from a remote's first URL and pushes to its last.
func TestNetwork_DialsOnlyTheValidatedURL(t *testing.T) {
	ts := newTwinServer(t)
	ctx := context.Background()
	n := ts.network(t, "")
	wt, _ := osWorktree(t)
	r, err := Clone(ctx, wt, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if u, err := r.RemoteURL("origin"); err != nil || u != n.URL {
		t.Fatalf("RemoteURL = %q, %v", u, err)
	}
	cfg := read(t, wt, ".git/config")
	edited := strings.Replace(cfg, "url = "+n.URL+"\n", "url = "+ts.otherURL+"\n\turl = "+n.URL+"\n\turl = "+ts.otherURL+"\n", 1)
	if edited == cfg {
		t.Fatalf("the config does not record the URL:\n%s", cfg)
	}
	if err := util.WriteFile(wt, ".git/config", []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err = Open(wt); err != nil {
		t.Fatal(err)
	}
	if u, _ := r.RemoteURL("origin"); u != ts.otherURL {
		t.Fatalf("the edited config's first URL is %q, want other.git", u)
	}
	ts.touchedOther()
	otherMain := run(t, ts.otherBare, "rev-parse", "main")

	next := ts.commit(t, "new.txt", "new\n")
	if res, err := r.Fetch(ctx, n, FetchOptions{}); err != nil || len(res.Updated) != 1 || res.Updated[0].New != next {
		t.Errorf("Fetch = %+v, %v; want origin/main at %s", res, err, next)
	}
	if p := ts.touchedOther(); len(p) > 0 {
		t.Errorf("Fetch requested other.git: %v", p)
	}
	next = ts.commit(t, "newer.txt", "newer\n")
	if res, err := r.Pull(ctx, n, PullOptions{}); err != nil || res.Head != next {
		t.Errorf("Pull = %+v, %v; want head %s", res, err, next)
	}
	if p := ts.touchedOther(); len(p) > 0 {
		t.Errorf("Pull requested other.git: %v", p)
	}
	write(t, wt, "mine.txt", "m\n")
	mine := commitAll(t, r, newClock(), "mine")
	if _, err := r.Push(ctx, n, PushOptions{}); err != nil {
		t.Errorf("Push: %v", err)
	}
	if p := ts.touchedOther(); len(p) > 0 {
		t.Errorf("Push requested other.git: %v", p)
	}
	if got := run(t, ts.bare, "rev-parse", "main"); got != mine {
		t.Errorf("app.git's main = %s, want the pushed %s", got, mine)
	}
	if got := run(t, ts.otherBare, "rev-parse", "main"); got != otherMain {
		t.Errorf("other.git's main moved to %s", got)
	}
}

// A fetch without refspecs uses the default ones, never the remote's
// configured fetch line: that line is the agent's to write, and one that
// writes refs/heads/* would move the checked-out branch under the worktree.
func TestFetch_WithoutRefspecsIgnoresTheConfiguredOnes(t *testing.T) {
	s := newServer(t, false)
	ctx := context.Background()
	n := s.network(t, "")
	wt, _ := osWorktree(t)
	if _, err := Clone(ctx, wt, n, CloneOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := read(t, wt, ".git/config")
	edited := strings.Replace(cfg, "fetch = +refs/heads/*:refs/remotes/origin/*", "fetch = +refs/heads/*:refs/heads/*", 1)
	if edited == cfg {
		t.Fatalf("the config has no default fetch line:\n%s", cfg)
	}
	if err := util.WriteFile(wt, ".git/config", []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(wt)
	if err != nil {
		t.Fatal(err)
	}
	next := s.commit(t, "new.txt", "new\n")
	res, err := r.Fetch(ctx, n, FetchOptions{})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if h, _ := r.Head(); h.Branch != "main" || h.Hash != s.head {
		t.Errorf("the fetch moved the checked-out branch: %+v", h)
	}
	want := RefUpdate{Name: "refs/remotes/origin/main", Old: s.head, New: next}
	if len(res.Updated) != 1 || res.Updated[0] != want {
		t.Errorf("Fetch updated %+v, want [%+v]", res.Updated, want)
	}
}

// A tag the remote has at another commit than the local one is kept, with
// or without tags, and reported; only a "+" refspec naming it moves it.
func TestFetch_KeepsALocalTagTheRemoteMoved(t *testing.T) {
	s := newServer(t, false)
	ctx := context.Background()
	n := s.network(t, "")
	run(t, s.work, "tag", "v1")
	run(t, s.work, "tag", "v2")
	run(t, s.work, "push", "-q", "served", "v1", "v2")
	wt, _ := osWorktree(t)
	r, err := Clone(ctx, wt, n, CloneOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v1 := plumbing.NewTagReferenceName("v1")
	for _, tags := range []bool{false, true} {
		if err := r.repo.Storer.SetReference(plumbing.NewHashReference(v1, plumbing.NewHash(s.first))); err != nil {
			t.Fatal(err)
		}
		s.commit(t, "f.txt", "tags "+map[bool]string{false: "off", true: "on"}[tags]+"\n")
		res, err := r.Fetch(ctx, n, FetchOptions{Tags: tags})
		if err != nil {
			t.Fatalf("Fetch tags=%v: %v", tags, err)
		}
		if ref, err := r.repo.Reference(v1, false); err != nil || ref.Hash().String() != s.first {
			t.Errorf("Fetch tags=%v moved the local tag v1: %v, %v", tags, ref, err)
		}
		if len(res.KeptTags) != 1 || res.KeptTags[0] != "v1" {
			t.Errorf("Fetch tags=%v reported kept tags %v, want [v1]", tags, res.KeptTags)
		}
		for _, u := range res.Updated {
			if u.Name == v1.String() {
				t.Errorf("Fetch tags=%v reported v1 as updated: %+v", tags, u)
			}
		}
	}
	res, err := r.Fetch(ctx, n, FetchOptions{RefSpecs: []string{"+refs/tags/v1:refs/tags/v1"}})
	if err != nil || len(res.KeptTags) != 0 {
		t.Errorf("a forced tag refspec = %+v, %v", res, err)
	}
	if ref, _ := r.repo.Reference(v1, false); ref == nil || ref.Hash().String() != s.head {
		t.Errorf("a forced tag refspec did not move v1: %v", ref)
	}
	if _, err := r.Fetch(ctx, n, FetchOptions{RefSpecs: []string{"refs/heads/x:refs/heads/main"}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a refspec writing the checked-out branch: %v", err)
	}
}

// A clone whose HEAD's tree is over the limits is refused before anything is
// checked out, with or without a checkout, and leaves nothing behind.
func TestClone_RefusesATreeOverTheLimitsAndLeavesNothing(t *testing.T) {
	s := newServer(t, false) // README.md, src and src/a.go: three entries
	ctx := context.Background()
	for _, noCheckout := range []bool{false, true} {
		wt, dir := osWorktree(t)
		write(t, wt, "mine.txt", "keep\n")
		_, err := Clone(ctx, wt, s.network(t, ""), CloneOptions{NoCheckout: noCheckout, Limits: Limits{MaxFiles: 2}})
		if Class(err) != "source_too_large" {
			t.Errorf("a clone (no checkout %v) over the file limit: %v, want source_too_large", noCheckout, err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 || read(t, wt, "mine.txt") != "keep\n" {
			t.Errorf("the refused clone (no checkout %v) left %d entries", noCheckout, len(entries))
		}
	}
	wt, _ := osWorktree(t)
	r, err := Clone(ctx, wt, s.network(t, ""), CloneOptions{Limits: Limits{MaxFiles: 3}})
	if err != nil || read(t, wt, "src/a.go") != "package a\n" {
		t.Fatalf("a clone within the limits: %v", err)
	}
	if r.limits.MaxFiles != 3 {
		t.Errorf("the cloned Repo's limits = %+v", r.limits)
	}
}
