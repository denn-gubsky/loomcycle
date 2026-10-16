package builtin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialtest"
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
	credStore  *sqlite.Store // the engine's store, to put a row in a state create refuses
	root       string
	registered []string // what the redactor was told to mask
}

func newGitSourceFixture(t *testing.T, requireAuth bool) *gitSourceFixture {
	t.Helper()
	if requireAuth {
		return newGitSourceFixtureFor(t, gitTestUser)
	}
	return newGitSourceFixtureFor(t, "")
}

// newGitSourceFixtureFor serves a repository that requires user and the test
// token as basic auth, or nothing when user is "".
func newGitSourceFixtureFor(t *testing.T, user string) *gitSourceFixture {
	t.Helper()
	opt := gitsourcetest.Options{}
	if user != "" {
		opt = gitsourcetest.Options{User: user, Token: gitTestToken}
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
	f.credStore = st

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
	if _, err := f.engine.PutInline(context.Background(), credential.Identity{TenantID: "tnt", Scope: "tenant", Name: name}, value, nil, norm, nil); err != nil {
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
	if stores, _ := filepath.Glob(filepath.Join(f.root, "_ephemeral", "run-1", ".gitstore-"+name+"*")); len(stores) != 0 {
		t.Errorf("the object store is still there: %v", stores)
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

// Two creates of one name can both pass the duplicate check before either
// records its volume. The second then found the first's directory already
// made, used it, failed on it (it is not empty), and its cleanup removed the
// whole directory: the first's volume, recorded or about to be. A directory
// this call did not make is refused and left alone.
func TestVolumeDefGitSource_ADirectoryAnotherCreateMadeIsLeftAlone(t *testing.T) {
	f := newGitSourceFixture(t, false)
	theirs := filepath.Join(f.volumeDir("repo"), "theirs.txt")
	if err := os.MkdirAll(filepath.Dir(theirs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theirs, []byte("written by the other create"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head}, nil)
	if !res.IsError || !strings.Contains(res.Text, "already exists") {
		t.Errorf("a create over a directory another create made: %q, want it refused as a duplicate", res.Text)
	}
	if b, err := os.ReadFile(theirs); err != nil || string(b) != "written by the other create" {
		t.Errorf("the other create's file: %q (err %v), want it untouched", b, err)
	}
	if n := f.repo.Requests.Load(); n != 0 {
		t.Errorf("%d request(s) were made for a name that is taken", n)
	}
}

// Two creates of one name at once, with room for both to run: at most one
// gets the volume, and the one that does keeps it whole.
func TestVolumeDefGitSource_TwoCreatesOfOneNameAtOnce(t *testing.T) {
	f := newGitSourceFixture(t, false)
	f.tool.Git.Slots = make(chan struct{}, 2)
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("repo%d", i)
		var wg sync.WaitGroup
		results := make([]tools.Result, 2)
		for j := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, _ := json.Marshal(map[string]any{"op": "create", "name": name, "ephemeral": true,
					"source": map[string]any{"git": map[string]any{"url": f.repo.URL, "commit": f.repo.Head}}})
				res, err := f.tool.Execute(f.ctx, json.RawMessage(body))
				if err != nil {
					res = tools.Result{IsError: true, Text: err.Error()}
				}
				results[j] = res
			}()
		}
		wg.Wait()
		ok := 0
		for _, r := range results {
			if !r.IsError {
				ok++
			}
		}
		if ok != 1 {
			t.Errorf("%s: %d of two creates succeeded, want exactly one: %q / %q", name, ok, results[0].Text, results[1].Text)
			continue
		}
		if !f.set.Has(name) {
			t.Errorf("%s: the volume that was created is not in the run's set", name)
		}
		if _, err := os.Stat(filepath.Join(f.volumeDir(name), "_meta", "omitted.json")); err != nil {
			t.Errorf("%s: the volume that was created is not whole: %v", name, err)
		}
		stores, _ := filepath.Glob(filepath.Join(f.root, "_ephemeral", "run-1", ".gitstore-"+name+"*"))
		if len(stores) != 0 {
			t.Errorf("%s: object stores left: %v", name, stores)
		}
	}
}

// gitEchoingRefusal answers every request with a 401 whose body echoes the
// request's credentials, as a careless or hostile server may.
type gitEchoingRefusal struct{}

func (gitEchoingRefusal) RoundTrip(r *http.Request) (*http.Response, error) {
	user, pass, _ := r.BasicAuth()
	body := "denied for " + user + ":" + pass + " (" + r.Header.Get("Authorization") + ")"
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// The operator's log gets the class, the status and the start of what the
// server said, never the token: a server that echoes the request's
// credentials in its refusal does not put them in the log.
func TestVolumeDefGitSource_ARefusalEchoingTheTokenIsLoggedWithoutIt(t *testing.T) {
	f := newGitSourceFixture(t, true)
	f.tool.Git.Transport = gitEchoingRefusal{}
	ref := f.credential(t, "repo", gitTestToken, &[]string{f.repo.URL})

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	_, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": ref}, nil)
	log.SetOutput(old)

	if !res.IsError || !strings.Contains(res.Text, "auth_failed") {
		t.Fatalf("create: %q, want auth_failed", res.Text)
	}
	logged := buf.String()
	basic := base64.StdEncoding.EncodeToString([]byte(gitTestUser + ":" + gitTestToken))
	if strings.Contains(logged, gitTestToken) || strings.Contains(logged, basic) {
		t.Errorf("the log carries the token: %s", logged)
	}
	if !strings.Contains(logged, "class=auth_failed") || !strings.Contains(logged, "401") {
		t.Errorf("the log does not say what happened: %s", logged)
	}
	f.assertNothingLeft(t, "repo")
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
	// A whole clone now, not a wait: the 150ms that bounded the wait above is
	// shorter than a clone takes under the race detector.
	f.tool.Git.Timeout = 30 * time.Second
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

// A $cred: reference to a credential that holds a GitHub App config is
// refused: sent as a token, the config (the App's private key with it) would
// go to the forge as the Basic-auth password, and be registered as a secret
// only after the fact. The same credential named as $ghapp: is minted.
func TestGitCredentialResolver_AnAppConfigNamedAsCredIsRefused(t *testing.T) {
	f := newGitSourceFixture(t, false)
	target := "https://github.com/acme/app"
	cfg := `{"app_id": 12, "installation_id": "34", "private_key": "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----\n"}`
	f.credential(t, "app", cfg, &[]string{target})
	minted := false
	resolve := GitCredentialResolver(f.engine, func(context.Context, string, string, string) (string, error) {
		minted = true
		return "ghs_minted_installation_token", nil
	}, nil)

	_, token, err := resolve(f.ctx, "$cred:app", target)
	if !errors.Is(err, ErrGitCredentialUnresolvable) || token != "" {
		t.Errorf("$cred: naming an App config: token=%q err=%v, want it unresolvable", token, err)
	}
	if err != nil && (strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "MIIE")) {
		t.Errorf("the refusal carries the stored config: %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "$ghapp:") {
		t.Errorf("the refusal does not point to $ghapp: %v", err)
	}
	if _, token, err := resolve(f.ctx, "$ghapp:app", target); err != nil || !minted || token != "ghs_minted_installation_token" {
		t.Errorf("$ghapp: of the same credential: token=%q err=%v minted=%v", token, err, minted)
	}
	// Through the tool, the caller is told the fix, and nothing of the config.
	f.tool.Git.Credential = resolve
	appRef := f.credential(t, "repo-app", cfg, &[]string{f.repo.URL})
	_, res := f.create(t, f.ctx, "repo", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": appRef}, nil)
	if !res.IsError || !strings.Contains(res.Text, "credential_unresolvable") || !strings.Contains(res.Text, "$ghapp:") || strings.Contains(res.Text, "MIIE") {
		t.Errorf("create with $cred: naming an App config: %q", res.Text)
	}
	f.assertNothingLeft(t, "repo")
	// A token that merely looks like JSON is not an App config.
	f.credential(t, "jsonish", `{"note":"not an app"}`, &[]string{target})
	if _, token, err := resolve(f.ctx, "$cred:jsonish", target); err != nil || token != `{"note":"not an app"}` {
		t.Errorf("a JSON value without app_id and private_key: token=%q err=%v", token, err)
	}
}

// A clone credential past its expires_at is unresolvable: neither a $cred:
// token nor a $ghapp: App config is handed out, and no App token is minted
// from an expired config.
func TestGitCredentialResolver_AnExpiredCredentialIsUnresolvable(t *testing.T) {
	f := newGitSourceFixture(t, false)
	target := "https://github.com/acme/app"
	for name, value := range map[string]string{"token": "glpat_stale", "app": `{"app_id":"1"}`} {
		f.credential(t, name, value, &[]string{target})
		credentialtest.Expire(t, f.credStore, credential.Identity{TenantID: "tnt", Scope: "tenant", Name: name}, time.Now().Add(-time.Minute))
	}
	minted := false
	var registered []string
	resolve := GitCredentialResolver(f.engine, func(context.Context, string, string, string) (string, error) {
		minted = true
		return "ghs_minted", nil
	}, func(v string) { registered = append(registered, v) })

	for _, ref := range []string{"$cred:token", "$ghapp:app"} {
		_, token, err := resolve(f.ctx, ref, target)
		if !errors.Is(err, ErrGitCredentialUnresolvable) || !strings.Contains(err.Error(), "expired") {
			t.Errorf("%s: err = %v, want it unresolvable because it expired", ref, err)
		}
		if token != "" {
			t.Errorf("%s: a token was returned for an expired credential", ref)
		}
	}
	if minted || len(registered) != 0 {
		t.Errorf("minted=%v registered=%d; want nothing minted or handed out", minted, len(registered))
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

// A credential's username is the basic-auth user name a clone sends. A forge
// that wants a particular name accepts the credential that carries it, and
// refuses the same token sent with the default name.
func TestVolumeDefGitSource_TheCredentialsUsernameIsSent(t *testing.T) {
	f := newGitSourceFixtureFor(t, "bob")
	allowed, err := credential.NormalizeAllowedURLs(&[]string{f.repo.URL})
	if err != nil {
		t.Fatal(err)
	}
	put := func(name string, username *string) {
		t.Helper()
		if _, err := f.engine.PutInline(context.Background(), credential.Identity{TenantID: "tnt", Scope: "tenant", Name: name}, gitTestToken, nil, allowed, username); err != nil {
			t.Fatal(err)
		}
	}
	bob := "bob"
	put("named", &bob)
	put("unnamed", nil)

	if _, res := f.create(t, f.ctx, "named", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": "$cred:named"}, nil); res.IsError {
		t.Errorf("the credential with the forge's user name: %s", res.Text)
	}
	if _, res := f.create(t, f.ctx, "unnamed", map[string]any{"url": f.repo.URL, "commit": f.repo.Head, "credential": "$cred:unnamed"}, nil); !res.IsError || !strings.Contains(res.Text, "auth_failed") {
		t.Errorf("the same token with the default user name: %s, want auth_failed", res.Text)
	}
}

// A run that may not use its user's own scope — a webhook's, or a schedule's
// whose author could not act as that user — does not clone with a credential
// stored in that scope, as no other $cred: it resolves does.
func TestGitCredentialResolver_ARunWithoutTheUserScopeDoesNotTakeTheUsersCredential(t *testing.T) {
	f := newGitSourceFixture(t, false)
	target := "https://github.com/acme/app"
	norm, err := credential.NormalizeAllowedURLs(&[]string{target})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.PutInline(context.Background(), credential.Identity{TenantID: "tnt", Scope: "user", ScopeID: "alice", Name: "personal"}, "glpat_alices_own", nil, norm, nil); err != nil {
		t.Fatal(err)
	}
	resolve := GitCredentialResolver(f.engine, nil, nil)
	asAlice := func(noUserScope bool) context.Context {
		return tools.WithRunIdentity(f.ctx, tools.RunIdentityValue{AgentID: "a", RootRunID: "run-1", TenantID: "tnt", UserID: "alice", CredentialsNoUserScope: noUserScope})
	}

	if _, token, err := resolve(asAlice(false), "$cred:personal", target); err != nil || token != "glpat_alices_own" {
		t.Fatalf("alice's own run: token=%q err=%v, want her credential", token, err)
	}
	_, token, err := resolve(asAlice(true), "$cred:personal", target)
	if !errors.Is(err, ErrGitCredentialUnresolvable) || token != "" {
		t.Errorf("a run without the user scope: token=%q err=%v, want it unresolvable", token, err)
	}
}
