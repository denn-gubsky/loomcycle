package builtin

import (
	"context"
	"crypto/tls"
	"encoding/json"
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

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/workspace/egress"
	"github.com/denn-gubsky/loomcycle/internal/workspace/lifecycle"
)

// The Workspace tool's open of a git workspace (RFC GW P11 plan §3.6): the
// run's checks, in the run, before any request reaches the forge.

const wsForgeToken = "tok-ws-forge-3d1a"

type wsForge struct {
	url      string
	requests atomic.Int64
	ca       *http.Transport
}

// newWSForge serves a repository with main (README.md) behind TLS, letting
// in only wsForgeToken.
func newWSForge(t *testing.T) *wsForge {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not installed")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "first")
	served := filepath.Join(base, "served")
	if err := os.MkdirAll(filepath.Join(served, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(base, "clone", "-q", "--bare", work, filepath.Join(served, "acme", "app.git"))
	h := &cgi.Handler{Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}}
	f := &wsForge{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if _, p, ok := r.BasicAuth(); !ok || p != wsForgeToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url, f.ca = srv.URL+"/acme/app.git", srv.Client().Transport.(*http.Transport)
	return f
}

// newWSGitFixture is a workspace fixture whose manager reaches the forge,
// with the Git tool's network settings listing the forge's host and a
// resolver that hands out the forge's token for "$cred:forge" only.
func newWSGitFixture(t *testing.T, forge *wsForge) *wsFixture {
	t.Helper()
	f := newWSFixture(t, lifecycle.Limits{})
	p, err := egress.NewGitProxy(egress.GitOptions{PrivateHostAllowlist: []string{"127.0.0.1/32"},
		TLSConfig: &tls.Config{RootCAs: forge.ca.TLSClientConfig.RootCAs, MinVersion: tls.VersionTLS12}})
	if err != nil {
		t.Fatal(err)
	}
	f.tool.Manager.SetGitProxy(p)
	f.h.D.SetGitEgress(f.tool.Manager.GitEgress)
	f.tool.GitSource = &GitSourceConfig{Hosts: []string{hostOf(t, forge.url)}, MaxBytes: 64 << 20, MaxObjectBytes: 1 << 20,
		Timeout: time.Minute, Transport: forge.ca, Slots: make(chan struct{}, 1),
		Credential: func(_ context.Context, ref, _ string) (string, string, error) {
			if ref != "$cred:forge" {
				return "", "", ErrGitCredentialURLNotAllowed
			}
			return "x-access-token", wsForgeToken, nil
		}}
	f.tool.GitPush = config.WorkspacePushConfig{}.Resolved()
	return f
}

func gitOpenPol() tools.WorkspacePolicyValue { return tools.WorkspacePolicyValue{Open: true} }

func withGitRemote(ctx context.Context, fetch bool) context.Context {
	return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.GitRemote = tools.GitRemotePolicyValue{Fetch: fetch} })
}

func gitOpenInput(forge *wsForge, extra map[string]any) string {
	in := map[string]any{"op": "open", "name": "app", "repo": forge.url, "credential": "$cred:forge"}
	for k, v := range extra {
		in[k] = v
	}
	b, _ := json.Marshal(in)
	return string(b)
}

func TestWorkspaceGit_OpenChecksOutTheRepositoryOnItsOwnBranch(t *testing.T) {
	forge := newWSForge(t)
	f := newWSGitFixture(t, forge)
	ctx := withGitRemote(f.runCtx(t, "alice", "", gitOpenPol()), true)
	res := f.mustOK(t, ctx, gitOpenInput(forge, nil))
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	if out["repo"] != forge.url || out["default_branch"] != "refs/heads/main" || !strings.HasPrefix(out["branch"].(string), "loom/app-") || len(out["base"].(string)) != 40 {
		t.Fatalf("open answered %s", res.Text)
	}
	if strings.Contains(res.Text, wsForgeToken) {
		t.Fatal("the answer holds the token")
	}
}

// Every check holds before any request reaches the forge.
func TestWorkspaceGit_OpenRefusesBeforeAnyRequest(t *testing.T) {
	forge := newWSForge(t)
	host := hostOf(t, forge.url)
	deny := func(ctx context.Context) context.Context {
		eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "acme", ID: "alice"},
			Bindings: []authz.Binding{{ACL: "git", SubjectKind: authz.SubjectTenant}},
			ACLs: map[string]authz.ACLVersion{"git": {ACL: authz.ACL{Name: "git", Rules: []authz.Rule{
				{Effect: authz.Deny, Actions: []string{"fetch"}, Resources: []string{"git:" + host + "/acme/*"}}}}}}, Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return tools.UpdateRunPolicy(ctx, func(p *tools.RunPolicy) { p.ACL = eff })
	}
	for name, c := range map[string]struct {
		ctx   func(f *wsFixture) context.Context
		tool  func(f *wsFixture)
		extra map[string]any
		want  string
	}{
		"no git_remote fetch": {ctx: func(f *wsFixture) context.Context {
			return withGitRemote(f.runCtx(t, "alice", "", gitOpenPol()), false)
		}, want: "git_remote_not_allowed"},
		"no git hosts":       {tool: func(f *wsFixture) { f.tool.GitSource = nil }, want: "git_network_disabled"},
		"an empty host list": {tool: func(f *wsFixture) { f.tool.GitSource.Hosts = nil }, want: "git_network_disabled"},
		"a host not listed": {tool: func(f *wsFixture) {
			f.tool.GitSource.Hosts = []string{"elsewhere.example"}
		}, want: "host_not_allowed"},
		"the user's rules deny fetch": {ctx: func(f *wsFixture) context.Context {
			return deny(withGitRemote(f.runCtx(t, "alice", "", gitOpenPol()), true))
		}, want: "acl_denied"},
		"a credential its resolver refuses": {extra: map[string]any{"credential": "$cred:unrestricted"}, want: "credential_url_not_allowed"},
		"not a repository URL":              {extra: map[string]any{"repo": "http://" + host + "/acme/app.git"}, want: "invalid_argument"},
		"a template too":                    {extra: map[string]any{"template": "t"}, want: "invalid_argument"},
		"an invalid branch name":            {extra: map[string]any{"branch": "a..b"}, want: "invalid_branch"},
		"over the MCP server": {ctx: func(f *wsFixture) context.Context {
			return withGitRemote(mcpCtx("acme", "alice"), true)
		}, want: "not_over_mcp"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newWSGitFixture(t, forge)
			if c.tool != nil {
				c.tool(f)
			}
			ctx := withGitRemote(f.runCtx(t, "alice", "", gitOpenPol()), true)
			if c.ctx != nil {
				ctx = c.ctx(f)
			}
			before := forge.requests.Load()
			res := f.call(t, ctx, gitOpenInput(forge, c.extra))
			if got := refusalClass(res); got != c.want {
				t.Fatalf("open = %s, want %s", res.Text, c.want)
			}
			if n := forge.requests.Load() - before; n != 0 {
				t.Fatalf("a refused open sent %d requests to the forge", n)
			}
		})
	}
}

// A credential the forge refuses opens nothing: the proof of access.
func TestWorkspaceGit_OpenWithACredentialTheForgeRefusesIsGitAccessDenied(t *testing.T) {
	forge := newWSForge(t)
	f := newWSGitFixture(t, forge)
	f.tool.GitSource.Credential = func(context.Context, string, string) (string, string, error) {
		return "x-access-token", "tok-the-forge-refuses", nil
	}
	ctx := withGitRemote(f.runCtx(t, "alice", "", gitOpenPol()), true)
	if res := f.call(t, ctx, gitOpenInput(forge, nil)); refusalClass(res) != "git_access_denied" {
		t.Fatalf("open = %s, want git_access_denied", res.Text)
	}
}
