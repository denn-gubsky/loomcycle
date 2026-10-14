package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitsourcetest"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	gitTestUser  = "x-access-token"
	gitTestToken = "ghs_the_test_token_0123456789"
)

// gitSourceFixture is a VolumeDef tool allowed to fetch from one served
// repository, a credential store, and a run that holds the grant.
type gitSourceFixture struct {
	tool       *VolumeDef
	ctx        context.Context
	set        *tools.EphemeralVolumeSet
	repo       *gitsourcetest.Repo
	engine     *credential.Engine
	root       string
	registered []string // what the redactor was told to mask
}

func newGitSourceFixture(t *testing.T, requireAuth bool) *gitSourceFixture {
	t.Helper()
	opt := gitsourcetest.Options{}
	if requireAuth {
		opt = gitsourcetest.Options{User: gitTestUser, Token: gitTestToken}
	}
	f := &gitSourceFixture{repo: gitsourcetest.New(t, opt)}
	tool, base, root, done := volumeDefFixture(t)
	t.Cleanup(done)
	f.tool, f.root = tool, root

	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(testKEK(), "")
	if err != nil {
		t.Fatal(err)
	}
	f.engine = credential.NewEngine(st, sealer)

	tool.Git = &GitSourceConfig{
		Hosts:    []string{f.repo.Host},
		MaxBytes: 16 << 20, MaxObjectBytes: 1 << 20, MaxFileBytes: 1000, MaxFiles: 1000,
		Timeout:    30 * time.Second,
		Slots:      make(chan struct{}, 1),
		Transport:  f.repo.Transport,
		Credential: GitCredentialResolver(f.engine, nil, func(v string) { f.registered = append(f.registered, v) }),
	}
	ctx, set := ephemeralCtx(base, "run-1")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", RootRunID: "run-1", TenantID: "tnt"})
	ctx = tools.WithAgentName(ctx, "reviewer")
	f.ctx = tools.WithVolumeDefPolicy(ctx, tools.VolumeDefPolicyValue{Scopes: []string{"any", GitSourceScope}})
	f.set = set
	return f
}

// credential stores a tenant credential and returns its reference.
func (f *gitSourceFixture) credential(t *testing.T, name, value string, allowed *[]string) string {
	t.Helper()
	norm, err := credential.NormalizeAllowedURLs(allowed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.PutInline(context.Background(), credential.Identity{TenantID: "tnt", Scope: "tenant", Name: name}, value, nil, norm); err != nil {
		t.Fatal(err)
	}
	return "$cred:" + name
}

// create calls the tool with a git source and returns the decoded answer.
func (f *gitSourceFixture) create(t *testing.T, ctx context.Context, name string, git map[string]any, extra map[string]any) (map[string]any, tools.Result) {
	t.Helper()
	in := map[string]any{"op": "create", "name": name, "ephemeral": true, "source": map[string]any{"git": git}}
	for k, v := range extra {
		in[k] = v
	}
	body, _ := json.Marshal(in)
	return vdExec(t, f.tool, ctx, string(body))
}

func (f *gitSourceFixture) volumeDir(name string) string {
	return filepath.Join(f.root, "_ephemeral", "run-1", name)
}

// assertNothingLeft checks that a refused or failed create left no directory,
// no object store, no row and no entry in the run's set, and that the name
// is free.
func (f *gitSourceFixture) assertNothingLeft(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(f.volumeDir(name)); !os.IsNotExist(err) {
		t.Errorf("the volume's directory is still there (err %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "_ephemeral", "run-1", ".gitstore-"+name)); !os.IsNotExist(err) {
		t.Errorf("the object store is still there (err %v)", err)
	}
	if f.set.Has(name) {
		t.Error("the run's set holds the volume")
	}
	rows, err := f.tool.Store.EphemeralVolumeListByRun(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == name {
			t.Error("a row records the volume")
		}
	}
}

func TestVolumeDefGitSource_ClonesACommitAndTheTokenStaysOutOfEverything(t *testing.T) {
	f := newGitSourceFixture(t, true)
	ref := f.credential(t, "repo", gitTestToken, &[]string{f.repo.URL})

	out, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": ref}, map[string]any{"mode": "ro"})
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	src, _ := out["source"].(map[string]any)
	if src["head"] != f.repo.Head || src["files"] != float64(6) || src["omitted"] != float64(4) {
		t.Errorf("source = %v, want the head, 6 files written and 4 paths left out", src)
	}
	if out["mode"] != "ro" || out["ephemeral"] != true {
		t.Errorf("answer = %v", out)
	}
	if strings.Contains(res.Text, gitTestToken) || strings.Contains(res.Text, "package a") {
		t.Errorf("the answer carries the token or file content: %s", res.Text)
	}

	// The agents of the run read it and, read-only as asked, cannot change it.
	if r := fileCall(t, f.ctx, &Read{}, map[string]any{"volume": "repo", "path": "src/a.go"}); r.IsError || r.Text != "package a\n" {
		t.Errorf("Read src/a.go: %q (error %v)", r.Text, r.IsError)
	}
	if r := fileCall(t, f.ctx, &Write{}, map[string]any{"volume": "repo", "path": "x", "content": "y"}); !r.IsError || !strings.Contains(r.Text, "read-only") {
		t.Errorf("a write to the read-only clone: %q", r.Text)
	}
	// What was left out is listed where an agent can read it.
	listed := fileCall(t, f.ctx, &Read{}, map[string]any{"volume": "repo", "path": "_meta/omitted.json"})
	for _, want := range []string{`"escape"`, `"absolute"`, `"vendor/lib"`, `"big.bin"`, `"too_large"`, `"symlink"`, `"submodule"`, `"count": 4`} {
		if !strings.Contains(listed.Text, want) {
			t.Errorf("_meta/omitted.json lacks %s:\n%s", want, listed.Text)
		}
	}

	// The token is nowhere a reader of the volume, or of the disk around it,
	// could find it: no file, no leftover store, no .git.
	dir := f.volumeDir("repo")
	_ = filepath.WalkDir(filepath.Join(f.root, "_ephemeral"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".git") {
			t.Errorf("git's own files are on disk at %s", p)
		}
		if d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), gitTestToken) {
				t.Errorf("the token is in %s", p)
			}
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Errorf("the tree is not at the volume's path: %v", err)
	}
	// The redactor was told about the token, so a later echo is masked.
	if len(f.registered) != 1 || f.registered[0] != gitTestToken {
		t.Errorf("registered with the redactor: %d value(s), want the token once", len(f.registered))
	}
	// A second create of the same name is the usual duplicate.
	if _, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": ref}, nil); !res.IsError || !strings.Contains(res.Text, "already exists") {
		t.Errorf("a second create of the name: %q", res.Text)
	}
}

func TestVolumeDefGitSource_APublicRepositoryNeedsNoCredential(t *testing.T) {
	f := newGitSourceFixture(t, false)
	out, res := f.create(t, f.ctx, "pub", map[string]any{"url": f.repo.URL, "commit": f.repo.Head}, nil)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	if out["mode"] != "rw" {
		t.Errorf("mode = %v, want the default rw", out["mode"])
	}
	if len(f.registered) != 0 {
		t.Error("a clone with no credential registered a secret")
	}
}

// Every one of these is refused before any request is made, and leaves
// nothing behind.
func TestVolumeDefGitSource_RefusedWithNoNetworkCall(t *testing.T) {
	f := newGitSourceFixture(t, true)
	good := f.credential(t, "repo", gitTestToken, &[]string{f.repo.URL})
	unrestricted := f.credential(t, "open", gitTestToken, nil)
	elsewhere := f.credential(t, "other", gitTestToken, &[]string{"https://" + f.repo.Host + "/acme/other"})
	nowhere := f.credential(t, "nowhere", gitTestToken, &[]string{})
	git := func(over map[string]any) map[string]any {
		m := map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": good}
		for k, v := range over {
			if v == nil {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	noGrant := tools.WithVolumeDefPolicy(f.ctx, tools.VolumeDefPolicyValue{Scopes: []string{"any"}})

	for _, c := range []struct {
		name  string
		ctx   context.Context
		git   map[string]any
		extra map[string]any
		setup func()
		want  string
	}{
		{name: "the agent lacks source:git (any does not imply it)", ctx: noGrant, git: git(nil), want: GitSourceScope},
		{name: "the host is not on the operator's list", ctx: f.ctx, git: git(map[string]any{"url": "https://evil.example/acme/app"}), want: "host_not_allowed"},
		{name: "a credential with no allowed_urls", ctx: f.ctx, git: git(map[string]any{"credential": unrestricted}), want: "credential_url_not_allowed"},
		{name: "a credential allowed for another repository", ctx: f.ctx, git: git(map[string]any{"credential": elsewhere}), want: "credential_url_not_allowed"},
		{name: "a credential that is sent nowhere", ctx: f.ctx, git: git(map[string]any{"credential": nowhere}), want: "credential_url_not_allowed"},
		{name: "a credential that does not exist", ctx: f.ctx, git: git(map[string]any{"credential": "$cred:missing"}), want: "credential_unresolvable"},
		{name: "a credential given as a value", ctx: f.ctx, git: git(map[string]any{"credential": gitTestToken}), want: "credential_unresolvable"},
		{name: "a field this build does not know", ctx: f.ctx, git: git(map[string]any{"keep_git": true}), want: "invalid_request"},
		{name: "an http URL", ctx: f.ctx, git: git(map[string]any{"url": "http://" + f.repo.Host + "/acme/app.git"}), want: "invalid_request"},
		{name: "not ephemeral", ctx: f.ctx, git: git(nil), extra: map[string]any{"ephemeral": false}, want: "invalid_request"},
		{name: "a negative max_bytes", ctx: f.ctx, git: git(map[string]any{"max_bytes": -1}), want: "invalid_request"},
	} {
		before := f.repo.Requests.Load()
		_, res := f.create(t, c.ctx, "repo", c.git, c.extra)
		if !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: %q (error %v), want a refusal naming %q", c.name, res.Text, res.IsError, c.want)
		}
		if n := f.repo.Requests.Load() - before; n != 0 {
			t.Errorf("%s: %d request(s) reached the server", c.name, n)
		}
		if strings.Contains(res.Text, gitTestToken) {
			t.Errorf("%s: the refusal carries the token", c.name)
		}
		f.assertNothingLeft(t, "repo")
	}

	// The feature unset: refused, whatever the grant.
	f.tool.Git = nil
	if _, res := f.create(t, f.ctx, "repo", git(nil), nil); !res.IsError || !strings.Contains(res.Text, "git_source_disabled") {
		t.Errorf("with the feature off: %q", res.Text)
	}
}

// A clone that fails after it has begun leaves nothing: the directory, the
// object store and the row are gone, and the name can be used again.
func TestVolumeDefGitSource_AFailedCloneLeavesNothingAndTheNameIsFree(t *testing.T) {
	f := newGitSourceFixture(t, true)
	wrong := f.credential(t, "stale", "ghs_a_revoked_token_999999", &[]string{f.repo.URL})
	right := f.credential(t, "repo", gitTestToken, &[]string{f.repo.URL})

	_, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": wrong}, nil)
	if !res.IsError || !strings.Contains(res.Text, "auth_failed") {
		t.Fatalf("a wrong token: %q, want auth_failed", res.Text)
	}
	// The model is told the class and nothing the remote said, nor the token.
	if strings.Contains(res.Text, "ghs_a_revoked") || strings.Contains(res.Text, "authentication required") || strings.Contains(res.Text, f.repo.URL) {
		t.Errorf("the refusal carries more than the class: %q", res.Text)
	}
	f.assertNothingLeft(t, "repo")

	// Over the byte limit: the same.
	tiny := map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": right, "max_bytes": 300}
	if _, res := f.create(t, f.ctx, "repo", tiny, nil); !res.IsError || !strings.Contains(res.Text, "source_too_large") {
		t.Errorf("over max_bytes: %q, want source_too_large", res.Text)
	}
	f.assertNothingLeft(t, "repo")

	// A commit the repository does not have.
	gone := map[string]any{"url": f.repo.URL, "commit": strings.Repeat("ab", 20), "credential": right}
	if _, res := f.create(t, f.ctx, "repo", gone, nil); !res.IsError || !strings.Contains(res.Text, "commit_not_found") {
		t.Errorf("an unknown commit: %q, want commit_not_found", res.Text)
	}
	f.assertNothingLeft(t, "repo")

	// And the name is free: the right credential now succeeds under it.
	if _, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": right}, nil); res.IsError {
		t.Errorf("after the failures the name should be free: %s", res.Text)
	}
}

// A request may lower the operator's byte limit and can never raise it.
func TestVolumeDefGitSource_MaxBytesOnlyLowers(t *testing.T) {
	f := newGitSourceFixture(t, false)
	f.tool.Git.MaxBytes = 300 // below what the fetch needs
	huge := map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "max_bytes": 1 << 40}
	if _, res := f.create(t, f.ctx, "repo", huge, nil); !res.IsError || !strings.Contains(res.Text, "source_too_large") {
		t.Errorf("a request asking for more than the operator allows: %q, want the operator's limit to hold", res.Text)
	}
}

// One clone at a time: a second waits for the slot, and gives up at the
// deadline with nothing left behind.
func TestVolumeDefGitSource_WaitsForASlotUntilTheDeadline(t *testing.T) {
	f := newGitSourceFixture(t, false)
	f.tool.Git.Timeout = 150 * time.Millisecond
	f.tool.Git.Slots <- struct{}{} // another clone holds the only slot
	start := time.Now()
	_, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head}, nil)
	if !res.IsError || !strings.Contains(res.Text, "timeout") {
		t.Errorf("with the slot taken: %q, want timeout", res.Text)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %s for a 150ms deadline", d)
	}
	if n := f.repo.Requests.Load(); n != 0 {
		t.Errorf("%d request(s) were made without a slot", n)
	}
	f.assertNothingLeft(t, "repo")
	<-f.tool.Git.Slots
	if _, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head}, nil); res.IsError {
		t.Errorf("once the slot is free: %s", res.Text)
	}
}

func TestGitCredentialResolver_AppCredentialIsMintedAndAnOpenOneRefused(t *testing.T) {
	f := newGitSourceFixture(t, false)
	target := "https://github.com/acme/app"
	f.credential(t, "app", `{"app_id":"1"}`, &[]string{target})
	f.credential(t, "open-app", `{"app_id":"1"}`, nil)

	var mintedFrom string
	var registered []string
	resolve := GitCredentialResolver(f.engine, func(_ context.Context, tenantID, cfgJSON, repository string) (string, error) {
		mintedFrom = tenantID + ":" + cfgJSON + ":" + repository
		return "ghs_minted_installation_token", nil
	}, func(v string) { registered = append(registered, v) })

	user, token, err := resolve(f.ctx, "$ghapp:app", target)
	if err != nil || user != "x-access-token" || token != "ghs_minted_installation_token" {
		t.Fatalf("an App credential: user=%q token=%q err=%v", user, token, err)
	}
	if mintedFrom != `tnt:{"app_id":"1"}:app` {
		t.Errorf("minted from %q, want the run's tenant, the stored config and the repository being cloned", mintedFrom)
	}
	// The minted token is what is masked, not the stored config.
	if len(registered) != 1 || registered[0] != token {
		t.Errorf("registered = %v, want the minted token", registered)
	}
	if _, _, err := resolve(f.ctx, "$ghapp:open-app", target); !errors.Is(err, ErrGitCredentialURLNotAllowed) {
		t.Errorf("an App credential with no allowed_urls: %v, want it refused", err)
	}
	for _, bad := range []string{"app", "$cred:", "$other:app", "$cred:a b", "prefix $cred:app", ""} {
		if _, _, err := resolve(f.ctx, bad, target); !errors.Is(err, ErrGitCredentialUnresolvable) {
			t.Errorf("reference %q: %v, want it unresolvable", bad, err)
		}
	}
	if got := gitAuthUsername("gitlab.example.com"); got != "oauth2" {
		t.Errorf("a GitLab host: user %q, want oauth2", got)
	}
	if got := gitAuthUsername("notgitlab.example.com"); got != "x-access-token" {
		t.Errorf("a host that only contains the word: user %q", got)
	}
}

func TestRepositoryName(t *testing.T) {
	for p, want := range map[string]string{
		"/acme/app": "app", "/acme/app.git": "app", "/acme/app/": "app", "/group/sub/app.git": "app", "/app": "app",
	} {
		if got := repositoryName(p); got != want {
			t.Errorf("repositoryName(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestHostListed(t *testing.T) {
	hosts := []string{"GitHub.com", "gitea.example:3000", " git.internal "}
	for host, want := range map[string]bool{
		"github.com": true, "GITHUB.COM": true, "github.com:443": true, "gitea.example:3000": true, "git.internal": true,
		"gitea.example": false, "github.com:8443": false, "api.github.com": false, "github.com.evil.example": false, "": false,
	} {
		if got := hostListed(hosts, host); got != want {
			t.Errorf("hostListed(%q) = %v, want %v", host, got, want)
		}
	}
}

// With a diff base the volume also holds the diff, under _meta/, and the
// answer names the base and the files.
func TestVolumeDefGitSource_WritesTheDiffAgainstABase(t *testing.T) {
	f := newGitSourceFixture(t, false)
	head := f.repo.Commit(t, "NEW.md", []byte("added after the fixture's head\n"))

	out, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": head, "diff_base": f.repo.Head}, map[string]any{"mode": "ro"})
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	src, _ := out["source"].(map[string]any)
	outputs, _ := src["outputs"].(map[string]any)
	if src["base"] != f.repo.Head || src["changed_files"] != float64(1) ||
		outputs["diff"] != "_meta/diff.patch" || outputs["numstat"] != "_meta/numstat.json" {
		t.Errorf("source = %v", src)
	}
	patch := fileCall(t, f.ctx, &Read{}, map[string]any{"volume": "repo", "path": "_meta/diff.patch"})
	if !strings.Contains(patch.Text, "+added after the fixture's head") || !strings.Contains(patch.Text, "NEW.md") {
		t.Errorf("the patch:\n%s", patch.Text)
	}
	stat := fileCall(t, f.ctx, &Read{}, map[string]any{"volume": "repo", "path": "_meta/numstat.json"})
	if !strings.Contains(stat.Text, `"path": "NEW.md"`) || !strings.Contains(stat.Text, `"status": "added"`) {
		t.Errorf("numstat.json:\n%s", stat.Text)
	}

	// merge-base:default, with the commit on the default branch itself: the
	// base is the commit, and the diff is empty.
	out, res = f.create(t, f.ctx, "same", map[string]any{"url": f.repo.URL, "commit": head, "diff_base": "merge-base:default",
		"outputs": map[string]any{"diff": "_meta/changes.patch", "numstat": "_meta/changes.json"}}, nil)
	if res.IsError {
		t.Fatalf("merge-base:default: %s", res.Text)
	}
	src, _ = out["source"].(map[string]any)
	outputs, _ = src["outputs"].(map[string]any)
	if src["base"] != head || src["changed_files"] != float64(0) || outputs["diff"] != "_meta/changes.patch" {
		t.Errorf("merge-base of the default branch's own tip: %v", src)
	}
	if r := fileCall(t, f.ctx, &Read{}, map[string]any{"volume": "same", "path": "_meta/changes.json"}); r.IsError {
		t.Errorf("the named numstat output is missing: %s", r.Text)
	}
}

func TestVolumeDefGitSource_DiffRefusals(t *testing.T) {
	f := newGitSourceFixture(t, false)
	for _, c := range []struct {
		name string
		git  map[string]any
		want string
	}{
		{"an output outside _meta", map[string]any{"diff_base": f.repo.Old, "outputs": map[string]any{"diff": "diff.patch"}}, "invalid_request"},
		{"an output that climbs out", map[string]any{"diff_base": f.repo.Old, "outputs": map[string]any{"diff": "_meta/../x.patch"}}, "invalid_request"},
		{"an output in a subdirectory", map[string]any{"diff_base": f.repo.Old, "outputs": map[string]any{"numstat": "_meta/a/b.json"}}, "invalid_request"},
		{"the omitted list as an output", map[string]any{"diff_base": f.repo.Old, "outputs": map[string]any{"diff": "_meta/omitted.json"}}, "invalid_request"},
		{"both outputs the same file", map[string]any{"diff_base": f.repo.Old, "outputs": map[string]any{"diff": "_meta/x", "numstat": "_meta/x"}}, "invalid_request"},
		{"outputs with no base", map[string]any{"outputs": map[string]any{"diff": "_meta/x.patch"}}, "invalid_request"},
		{"a base that is not a sha", map[string]any{"diff_base": "main"}, "invalid_request"},
		{"a base the repository does not have", map[string]any{"diff_base": strings.Repeat("ab", 20)}, "commit_not_found"},
	} {
		git := map[string]any{"url": f.repo.URL, "commit": f.repo.Head}
		for k, v := range c.git {
			git[k] = v
		}
		_, res := f.create(t, f.ctx, "repo", git, nil)
		if !res.IsError || !strings.Contains(res.Text, c.want) {
			t.Errorf("%s: %q, want %s", c.name, res.Text, c.want)
		}
		f.assertNothingLeft(t, "repo")
	}
}

func TestValidMetaOutput(t *testing.T) {
	for rel, want := range map[string]bool{
		"_meta/diff.patch": true, "_meta/numstat.json": true, "_meta/a_b-c.1": true,
		"_meta/omitted.json": false, "_meta/": false, "_meta/.hidden": false, "_meta/a/b": false,
		"diff.patch": false, "_META/diff.patch": false, "_meta/a b": false, "_meta/../x": false,
		"_meta/" + strings.Repeat("a", 65): false,
	} {
		if got := validMetaOutput(rel); got != want {
			t.Errorf("validMetaOutput(%q) = %v, want %v", rel, got, want)
		}
	}
}
