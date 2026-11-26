package lifecycle

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitrepo"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/worker"
)

// The git open tests run the whole way: a workspace worker's go-git, over
// the worker's own connection, through the core's git proxy and its op
// ticket, to a real git forge (git's http-backend behind TLS) that lets in
// only the token of the user it serves.

const (
	forgeUser   = "x-access-token"
	forgeTokenA = "tok-forge-A-6f02"
	// forgeTokenListOnly may list the repository's refs but not fetch.
	forgeTokenListOnly = "tok-forge-list-91c3"
)

type realForge struct {
	url  string // the repository's URL
	work string // a working copy that pushes to it
	bare string
	ca   *http.Transport
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRealForge serves a repository with main (README.md) and a branch
// feature, letting in only forgeTokenA, and forgeTokenListOnly to the
// advertisement alone.
func newRealForge(t *testing.T) *realForge {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-q", "-m", "first")
	gitRun(t, work, "branch", "feature")
	bare := filepath.Join(base, "served", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, base, "clone", "-q", "--bare", work, bare)
	gitRun(t, bare, "config", "http.receivepack", "true")
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(base, "served"), "GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if ok && u == forgeUser && p == forgeTokenListOnly && r.Method == http.MethodGet {
			h.ServeHTTP(w, r)
			return
		}
		if !ok || u != forgeUser || p != forgeTokenA {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &realForge{url: srv.URL + "/acme/app.git", work: work, bare: bare, ca: srv.Client().Transport.(*http.Transport)}
}

// newGitFixture is a fixture whose manager sends git requests to the forge.
func newGitFixture(t *testing.T, forge *realForge) *fixture {
	t.Helper()
	f := newFixture(t, Limits{MaxPerRun: 4}, worker.Options{})
	p, err := egress.NewGitProxy(egress.GitOptions{PrivateHostAllowlist: []string{"127.0.0.1/32"},
		TLSConfig: &tls.Config{RootCAs: forge.ca.TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	f.m.SetGitProxy(p)
	f.h.D.SetGitEgress(f.m.GitEgress)
	return f
}

func gitOpenReq(forge *realForge, name, branch, token string) OpenRequest {
	return OpenRequest{Name: name, Toolchains: []string{"python", "node"}, Git: &GitOpen{
		URL: forge.url, Branch: branch, Credential: "$cred:forge", Username: forgeUser, Token: token,
		MaxObjectBytes: 1 << 20, MaxReadBytes: 64 << 20, Timeout: time.Minute,
		Push: config.WorkspacePushConfig{}.Resolved(),
	}}
}

func TestOpenGit_ChecksOutTheBaseOnItsOwnBranchAndPinsIt(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if row.Kind != store.WorkspaceKindGit || row.Git == nil || row.State != store.WorkspaceReady {
		t.Fatalf("row = %+v", row)
	}
	g := row.Git
	if g.URL != forge.url || g.DefaultBranch != "refs/heads/main" || len(g.Base) != 40 || !strings.HasPrefix(g.Branch, "loom/app-") {
		t.Fatalf("git = %+v", g)
	}
	if g.Mirror != gitrepo.MirrorKey("acme", forge.url) {
		t.Fatalf("mirror key %q is not the tenant's", g.Mirror)
	}
	b, err := os.ReadFile(filepath.Join(f.h.Root, row.ID, "README.md"))
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("README.md = %q, %v", b, err)
	}
	if got := gitRun(t, filepath.Join(f.h.Root, ".loomcycle-mirrors", g.Mirror), "--git-dir=.", "for-each-ref", "--format=%(refname)", "refs/loom/"); !strings.Contains(got, row.ID) {
		t.Fatalf("no pin for the workspace: %q", got)
	}
	stored, err := f.st.WorkspaceGet(context.Background(), "acme", row.ID)
	if err != nil || stored.Git == nil || stored.Git.Base != g.Base {
		t.Fatalf("stored git = %+v, %v", stored.Git, err)
	}
}

// RFC GW §11 "Access is proved on open": a user the forge refuses cannot
// open a workspace on a repository whose mirror another user filled.
func TestOpenGit_AUserTheForgeRefusesCannotUseAMirrorAnotherFilled(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	if _, err := f.m.Open(context.Background(), f.run(t, "alice"), gitOpenReq(forge, "app", "", forgeTokenA)); err != nil {
		t.Fatalf("alice's open: %v", err)
	}
	ents, _ := os.ReadDir(f.h.Root)
	bob := f.run(t, "bob")
	_, err := f.m.Open(context.Background(), bob, gitOpenReq(forge, "app", "", "tok-bob-the-forge-refuses"))
	if !errors.Is(err, ErrGitAccess) {
		t.Fatalf("bob's open = %v, want ErrGitAccess", err)
	}
	if rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", bob.RunID); len(rows) != 0 {
		t.Fatalf("bob holds %d rows", len(rows))
	}
	after, _ := os.ReadDir(f.h.Root)
	if len(after) != len(ents) {
		t.Fatalf("bob's refused open left something on the worker: %d entries, were %d", len(after), len(ents))
	}
}

// An advertisement alone proves nothing: a credential the forge lets list
// the refs but not fetch cannot open a workspace on a mirror another user
// filled, though the mirror is up to date and the fetch itself needs no
// upload-pack. The core counts the forge's answers, not the worker's word.
func TestOpenGit_AnAdvertisementAloneDoesNotProveAccess(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	if _, err := f.m.Open(context.Background(), f.run(t, "alice"), gitOpenReq(forge, "app", "", forgeTokenA)); err != nil {
		t.Fatalf("alice's open: %v", err)
	}
	bob := f.run(t, "bob")
	if _, err := f.m.Open(context.Background(), bob, gitOpenReq(forge, "app", "", forgeTokenListOnly)); !errors.Is(err, ErrGitAccess) {
		t.Fatalf("bob's open = %v, want ErrGitAccess", err)
	}
	if rows, _ := f.st.WorkspaceListByOwnerRun(context.Background(), "acme", bob.RunID); len(rows) != 0 {
		t.Fatalf("bob holds %d rows", len(rows))
	}
}

// A second open of a repository whose mirror is up to date is proved by the
// forge's answer to an upload-pack that asks for nothing.
func TestOpenGit_ASecondOpenOnAnUpToDateMirrorIsProved(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	for _, name := range []string{"one", "two"} {
		if _, err := f.m.Open(context.Background(), f.run(t, "alice"), gitOpenReq(forge, name, "", forgeTokenA)); err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
	}
}

// The forge's default branch is never a workspace's own; a branch the forge
// already has is refused unless the operator adopts existing branches (the
// owner's Q2); a branch outside the operator's prefixes is refused.
func TestOpenGit_RefusesBranchesAPushMustNotMove(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	for name, c := range map[string]struct {
		branch string
		push   config.WorkspacePushConfig
		want   error
	}{
		"the default branch":      {"main", config.WorkspacePushConfig{}.Resolved(), ErrGitBranch},
		"a branch the forge has":  {"feature", config.WorkspacePushConfig{}.Resolved(), ErrGitBranchExists},
		"outside the prefixes":    {"other", config.WorkspacePushConfig{RefPrefixes: []string{"refs/heads/loom/"}}.Resolved(), ErrGitBranch},
		"not a valid branch name": {"a..b", config.WorkspacePushConfig{}.Resolved(), ErrGitBranch},
	} {
		t.Run(name, func(t *testing.T) {
			req := gitOpenReq(forge, "w"+strings.ReplaceAll(c.branch, ".", ""), c.branch, forgeTokenA)
			req.Git.Push = c.push
			if _, err := f.m.Open(context.Background(), o, req); !errors.Is(err, c.want) {
				t.Fatalf("open = %v, want %v", err, c.want)
			}
		})
	}
	adopt := gitOpenReq(forge, "adopt", "feature", forgeTokenA)
	adopt.Git.Push.AdoptExisting = true
	row, err := f.m.Open(context.Background(), o, adopt)
	if err != nil || row.Git.Branch != "feature" {
		t.Fatalf("adopting an existing branch: %+v, %v", row.Git, err)
	}
}

// Releasing a git workspace removes it and its pins from the mirror.
func TestReleaseGit_DropsTheWorkspacesPins(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	row, err := f.m.Open(context.Background(), f.run(t, "alice"), gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Release(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(f.h.Root, ".loomcycle-mirrors", row.Git.Mirror)
	if got := gitRun(t, mirror, "--git-dir=.", "for-each-ref", "--format=%(refname)", "refs/loom/"); got != "" {
		t.Fatalf("pins left after release: %q", got)
	}
}

// A git workspace is not forked or packed into a template until its own
// copy is made (P11 PR 9): a plain copy would carry its git directory and
// alternates to the mirror with no pin of its own.
func TestOpenGit_AForkOrTemplateOfAGitWorkspaceIsRefused(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	row, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Fork(context.Background(), o, row, "copy", ""); !errors.Is(err, ErrGitUnsupported) {
		t.Fatalf("fork = %v, want ErrGitUnsupported", err)
	}
	if _, err := f.m.SaveTemplate(context.Background(), o, row, "ck", "tpl", store.WorkspaceTemplateUser); !errors.Is(err, ErrGitUnsupported) {
		t.Fatalf("save template = %v, want ErrGitUnsupported", err)
	}
}

// A repeated open returns the workspace the run holds only when it is what
// was asked: a git open of a name the run holds as a general workspace, or
// on another repository, is refused rather than answered with the other.
func TestOpenGit_ARepeatedOpenOfAnotherKindIsRefused(t *testing.T) {
	forge := newRealForge(t)
	f := newGitFixture(t, forge)
	o := f.run(t, "alice")
	f.open(t, o, "plain")
	if _, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "plain", "", forgeTokenA)); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("git open of a general workspace's name = %v, want ErrNameTaken", err)
	}
	row, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.m.Open(context.Background(), o, gitOpenReq(forge, "app", "", forgeTokenA))
	if err != nil || again.ID != row.ID {
		t.Fatalf("repeated git open = %+v, %v", again, err)
	}
	other := gitOpenReq(forge, "app", "", forgeTokenA)
	other.Git.URL = strings.TrimSuffix(forge.url, "app.git") + "other.git"
	if _, err := f.m.Open(context.Background(), o, other); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("git open on another repository = %v, want ErrNameTaken", err)
	}
	if _, err := f.m.Open(context.Background(), o, OpenRequest{Name: "app", Toolchains: []string{"python", "node"}}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("general open of a git workspace's name = %v, want ErrNameTaken", err)
	}
}
