//go:build unix

package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/gitsource"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

// gitToolTestToken is the credential value the fake resolver hands out. Every
// network test checks it appears in no result, no file under the volume.
const gitToolTestToken = "tok-3f9c1e-never-shown"

type recordingSink struct {
	mu  sync.Mutex
	evs []audit.Event
}

func (s *recordingSink) Record(ev audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
	return nil
}

// gitCtx binds one volume and the git_remote authority.
func gitCtx(root string, readOnly bool, remote tools.GitRemotePolicyValue) context.Context {
	ctx := tools.WithRunPolicy(context.Background(), tools.RunPolicy{
		Volumes:   tools.VolumePolicyValue{Active: true, Bindings: []tools.VolumeBinding{{Name: "default", Root: root, Default: true, ReadOnly: readOnly}}},
		GitRemote: remote,
	})
	ctx = tools.WithRunID(ctx, "run-git-1")
	return ctx
}

func runGit(t *testing.T, g *Git, ctx context.Context, in map[string]any) (map[string]any, bool, string) {
	t.Helper()
	body, _ := json.Marshal(in)
	res, err := g.Execute(ctx, body)
	if err != nil {
		t.Fatalf("Execute %v: %v", in, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("result %q is not JSON: %v", res.Text, err)
	}
	return out, res.IsError, res.Text
}

func mustGitOK(t *testing.T, g *Git, ctx context.Context, in map[string]any) map[string]any {
	t.Helper()
	out, isErr, raw := runGit(t, g, ctx, in)
	if isErr {
		t.Fatalf("%v: %s", in["op"], raw)
	}
	return out
}

func wantGitClass(t *testing.T, g *Git, ctx context.Context, in map[string]any, class string) {
	t.Helper()
	out, isErr, raw := runGit(t, g, ctx, in)
	if !isErr || out["error"] != class {
		t.Fatalf("%v: got %s, want error %q", in["op"], raw, class)
	}
}

// Local work needs no network and no authority beyond the tool: init, a
// commit under the operator's identity with the run trailer, log, diff.
func TestGit_LocalCommitCarriesOperatorIdentityAndRunTrailer(t *testing.T) {
	root := realDir(t)
	g := &Git{AuthorName: "Release Bot", AuthorEmail: "bot@example.com"}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{})

	if out := mustGitOK(t, g, ctx, map[string]any{"op": "init", "repo": "svc", "initial_branch": "trunk"}); out["branch"] != "trunk" {
		t.Errorf("init reported branch %v, want trunk", out["branch"])
	}
	if err := os.WriteFile(filepath.Join(root, "svc", "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := mustGitOK(t, g, ctx, map[string]any{"op": "status", "repo": "svc"})
	if st["clean"] != false {
		t.Fatalf("status after a new file: %v", st)
	}
	mustGitOK(t, g, ctx, map[string]any{"op": "add", "repo": "svc", "paths": []string{"."}})
	c := mustGitOK(t, g, ctx, map[string]any{"op": "commit", "repo": "svc", "message": "first"})
	if c["hash"] == "" {
		t.Fatalf("commit: %v", c)
	}
	logOut := mustGitOK(t, g, ctx, map[string]any{"op": "log", "repo": "svc"})
	commits := logOut["commits"].([]any)
	first := commits[0].(map[string]any)
	if first["author"] != "Release Bot" || first["author_email"] != "bot@example.com" {
		t.Errorf("author: %v", first)
	}
	if !strings.Contains(first["message"].(string), "Loomcycle-Run: run-git-1") {
		t.Errorf("message has no run trailer: %q", first["message"])
	}
	if err := os.WriteFile(filepath.Join(root, "svc", "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := mustGitOK(t, g, ctx, map[string]any{"op": "diff", "repo": "svc"})
	if p := d["patch"].(string); !strings.Contains(p, "-one") || !strings.Contains(p, "+two") {
		t.Errorf("diff: %q", p)
	}
	wantGitClass(t, g, ctx, map[string]any{"op": "commit", "repo": "svc", "message": "nothing staged"}, "nothing_to_commit")
}

// A read-only volume allows the read ops and refuses every change.
func TestGit_ReadOnlyVolumeRefusesChanges(t *testing.T) {
	root := realDir(t)
	g := &Git{}
	mustGitOK(t, g, gitCtx(root, false, tools.GitRemotePolicyValue{}), map[string]any{"op": "init"})
	ro := gitCtx(root, true, tools.GitRemotePolicyValue{})
	mustGitOK(t, g, ro, map[string]any{"op": "status"})
	for _, op := range []string{"add", "commit", "reset", "clean", "branch_create"} {
		out, isErr, raw := runGit(t, g, ro, map[string]any{"op": op, "message": "m", "branch": "b", "paths": []string{"."}})
		if !isErr || out["error"] != "volume_refused" {
			t.Errorf("op %s on a read-only volume: %s", op, raw)
		}
	}
}

// A repository directory outside the volume, through a symlink or "..", is
// refused before the library sees it.
func TestGit_RepoOutsideTheVolumeIsRefused(t *testing.T) {
	root := realDir(t)
	outside := realDir(t)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	g := &Git{}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{})
	for _, repo := range []string{"../x", "link", "link/sub"} {
		if _, isErr, raw := runGit(t, g, ctx, map[string]any{"op": "init", "repo": repo}); !isErr {
			t.Errorf("init in %q: %s", repo, raw)
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("something was written outside the volume: %v", ents)
	}
}

// The network is gated three ways, each before any request: the agent's
// git_remote, the operator's host lists, the URL's scheme.
func TestGit_NetworkGatesRefuseBeforeAnyRequest(t *testing.T) {
	var requests int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	host := hostOf(t, srv.URL)
	src := &GitSourceConfig{Hosts: []string{host}, MaxBytes: 1 << 20, MaxObjectBytes: 1 << 20, Timeout: 10 * time.Second, Transport: srv.Client().Transport}
	root := realDir(t)
	fetch := tools.GitRemotePolicyValue{Fetch: true}
	both := tools.GitRemotePolicyValue{Fetch: true, Push: true}
	repoURL := srv.URL + "/acme/app.git"

	cases := []struct {
		name   string
		g      *Git
		remote tools.GitRemotePolicyValue
		in     map[string]any
		class  string
	}{
		{"no git_remote", &Git{Source: src}, tools.GitRemotePolicyValue{}, map[string]any{"op": "ls_remote", "url": repoURL}, "git_remote_not_allowed"},
		{"fetch only cannot push", &Git{Source: src, PushHosts: []string{host}}, fetch, map[string]any{"op": "push"}, "git_remote_not_allowed"},
		{"no hosts configured", &Git{}, fetch, map[string]any{"op": "ls_remote", "url": repoURL}, "git_network_disabled"},
		{"no push hosts", &Git{Source: src}, both, map[string]any{"op": "push"}, "git_push_disabled"},
		{"host not listed", &Git{Source: src}, fetch, map[string]any{"op": "ls_remote", "url": "https://elsewhere.example/acme/app.git"}, "host_not_allowed"},
		{"file scheme", &Git{Source: src}, fetch, map[string]any{"op": "ls_remote", "url": "file:///etc"}, "invalid_argument"},
		{"ssh scheme", &Git{Source: src}, fetch, map[string]any{"op": "ls_remote", "url": "ssh://" + host + "/acme/app.git"}, "invalid_argument"},
		{"plain http", &Git{Source: src}, fetch, map[string]any{"op": "ls_remote", "url": "http://" + host + "/acme/app.git"}, "invalid_argument"},
		{"user in url", &Git{Source: src}, fetch, map[string]any{"op": "ls_remote", "url": "https://x:" + gitToolTestToken + "@" + host + "/acme/app.git"}, "invalid_argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := requests
			wantGitClass(t, c.g, gitCtx(root, false, c.remote), c.in, c.class)
			if requests != before {
				t.Errorf("a refused call reached the server")
			}
		})
	}
}

// The full path: clone a private repository with a credential, commit,
// push. The token reaches the server, and nothing else: not a result, not a
// file under the volume, not the audit record (which names the reference).
func TestGit_CloneCommitPushKeepsTheCredentialOutOfReach(t *testing.T) {
	f := newGitServer(t, true)
	root := realDir(t)
	sink := &recordingSink{}
	var resolvedFor []string
	resolve := func(_ context.Context, ref, target string) (string, string, error) {
		resolvedFor = append(resolvedFor, ref+" "+target)
		return "x-access-token", gitToolTestToken, nil
	}
	pushResolved := 0
	g := &Git{
		Source:       f.source(resolve),
		PushHosts:    []string{f.host},
		PushMaxBytes: 1 << 20,
		PushCredential: func(ctx context.Context, ref, target string) (string, string, error) {
			pushResolved++
			return "x-access-token", gitToolTestToken, nil
		},
		Audit: sink,
	}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{Fetch: true, Push: true})
	var results []string
	do := func(in map[string]any) map[string]any {
		t.Helper()
		out, isErr, raw := runGit(t, g, ctx, in)
		results = append(results, raw)
		if isErr {
			t.Fatalf("%v: %s", in["op"], raw)
		}
		return out
	}

	do(map[string]any{"op": "clone", "repo": "app", "url": f.repoURL, "credential": "$cred:forge"})
	if len(resolvedFor) != 1 || resolvedFor[0] != "$cred:forge "+f.repoURL {
		t.Fatalf("the read resolver was asked for %v", resolvedFor)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	do(map[string]any{"op": "add", "repo": "app", "paths": []string{"new.txt"}})
	do(map[string]any{"op": "commit", "repo": "app", "message": "add new"})
	do(map[string]any{"op": "push", "repo": "app", "credential": "$cred:forge"})
	if pushResolved != 1 {
		t.Errorf("push resolved its credential %d times through the push resolver, want 1", pushResolved)
	}
	if got := f.git(t, "log", "-1", "--format=%s", "main"); got != "add new" {
		t.Errorf("the server's main is at %q, want the pushed commit", got)
	}
	if !f.sawToken() {
		t.Error("the server never saw the credential: the test proves nothing")
	}

	for _, r := range results {
		if strings.Contains(r, gitToolTestToken) {
			t.Errorf("a result carries the token: %s", r)
		}
	}
	assertNoFileHolds(t, root, gitToolTestToken)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.evs) != 2 {
		t.Fatalf("audit records: %d, want clone and push", len(sink.evs))
	}
	for _, ev := range sink.evs {
		b, _ := json.Marshal(ev)
		if bytes.Contains(b, []byte(gitToolTestToken)) {
			t.Errorf("an audit record carries the token: %s", b)
		}
		if ev.GitCredential != "$cred:forge" || ev.GitOutcome != "ok" || ev.GitRunID != "run-git-1" {
			t.Errorf("audit record: %+v", ev)
		}
	}
}

// A remote rewritten in .git/config — its url, or an insteadOf rule that
// redirects it — is checked as the library would read it: to a host the
// operator does not list it is refused, and so is a scheme other than https.
func TestGit_RemoteRewrittenInConfigIsCheckedAtUse(t *testing.T) {
	f := newGitServer(t, false)
	root := realDir(t)
	g := &Git{Source: f.source(nil)}
	ctx := gitCtx(root, false, tools.GitRemotePolicyValue{Fetch: true})
	mustGitOK(t, g, ctx, map[string]any{"op": "clone", "repo": "app", "url": f.repoURL})
	cfgPath := filepath.Join(root, "app", ".git", "config")
	orig, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(orig, []byte(gitToolTestToken)) {
		t.Fatal("the clone wrote a credential into .git/config")
	}
	for _, c := range []struct{ name, extra, class string }{
		{"insteadOf to another host", "[url \"https://evil.example/\"]\n\tinsteadOf = " + f.base + "/\n", "host_not_allowed"},
		{"insteadOf to file", "[url \"file:///etc/\"]\n\tinsteadOf = " + f.base + "/\n", "invalid_argument"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(cfgPath, append(append([]byte{}, orig...), c.extra...), 0o644); err != nil {
				t.Fatal(err)
			}
			before := f.requests()
			wantGitClass(t, g, ctx, map[string]any{"op": "fetch", "repo": "app"}, c.class)
			if f.requests() != before {
				t.Error("the rewritten remote was contacted")
			}
		})
	}
	if err := os.WriteFile(cfgPath, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitOK(t, g, ctx, map[string]any{"op": "fetch", "repo": "app"})
}

// The library is never handed a scheme other than https, even through a
// path the tool's own check does not cover: the process-wide clients for
// file, git, ssh and http refuse.
func TestGit_ProcessWideTransportsRefuseOtherSchemes(t *testing.T) {
	gitsource.InstallTransport()
	for _, scheme := range []string{"file", "git", "ssh", "http"} {
		if err := gitsourceProbe(scheme); err == nil || !strings.Contains(err.Error(), "only https") {
			t.Errorf("%s: %v, want the refusing client", scheme, err)
		}
	}
}

// gitsourceProbe asks the library's process-wide client for scheme to open
// a session, as go-git does for a remote with that scheme.
func gitsourceProbe(scheme string) error {
	raw := scheme + "://example.invalid/acme/app.git"
	if scheme == "file" {
		raw = "file:///tmp/app.git"
	}
	ep, err := transport.NewEndpoint(raw)
	if err != nil {
		return err
	}
	c, err := client.NewClient(ep)
	if err != nil {
		return err
	}
	_, err = c.NewUploadPackSession(ep, nil)
	return err
}

// ---- fixture ----

type gitServer struct {
	base, repoURL, host string
	bare                string
	srv                 *httptest.Server
	mu                  sync.Mutex
	n                   int
	token               bool
}

func (f *gitServer) requests() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.n }
func (f *gitServer) sawToken() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.token }

func (f *gitServer) source(resolve GitCredentialFunc) *GitSourceConfig {
	return &GitSourceConfig{
		Hosts: []string{f.host}, MaxBytes: 8 << 20, MaxObjectBytes: 4 << 20,
		Timeout: 30 * time.Second, Transport: f.srv.Client().Transport, Credential: resolve,
	}
}

func (f *gitServer) git(t *testing.T, args ...string) string {
	t.Helper()
	return gitRun(t, f.bare, args...)
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newGitServer serves one repository with git's http-backend behind TLS,
// push enabled. requireAuth refuses a request without the test token.
func newGitServer(t *testing.T, requireAuth bool) *gitServer {
	t.Helper()
	missing := t.Skipf
	if os.Getenv("CI") != "" {
		missing = t.Fatalf
	}
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		missing("SKIPPED, NOT PASSED: git is not installed: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		missing("SKIPPED, NOT PASSED: %s not found", backend)
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", ".")
	gitRun(t, work, "commit", "-q", "-m", "first")
	bare := filepath.Join(base, "served", "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, base, "clone", "-q", "--bare", work, bare)
	gitRun(t, bare, "config", "http.receivepack", "true")

	f := &gitServer{bare: bare}
	h := &cgi.Handler{Path: backend, Env: []string{
		"GIT_PROJECT_ROOT=" + filepath.Join(base, "served"), "GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "REMOTE_USER=pusher",
	}}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+gitToolTestToken))
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.n++
		if r.Header.Get("Authorization") == want {
			f.token = true
		}
		f.mu.Unlock()
		if requireAuth && r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	f.base = f.srv.URL
	f.repoURL = f.srv.URL + "/acme/app.git"
	f.host = hostOf(t, f.srv.URL)
	return f
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return gitsource.HostKey(u)
}

// assertNoFileHolds walks root (the volume) and fails on any file holding s.
func assertNoFileHolds(t *testing.T, root, s string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(b, []byte(s)) {
			t.Errorf("%s holds the credential", p)
		}
		return nil
	})
}
