package http

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/gitsource/gitsourcetest"
	"github.com/denn-gubsky/loomcycle/internal/providers/codejs"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The token canary for a volume made from a git source. A code-js run clones
// two private repositories, one with a $cred: token and one with a $ghapp:
// App credential, then makes more tool calls, including a Bash call. Each
// token is then looked for everywhere the run could have put it: the live
// stream, the database, the process log, every file under the volumes'
// root (the volume itself and whatever a clone leaves beside it), and the
// environment the Bash call ran with. It must be in none of them.
//
// The control (TestGitSourceCanary_ACloneThroughBashIsFound) does the clone
// the way a team did before: the token in Bash's environment and git run by
// the model. The same search finds it there, so a clean result above is a
// finding and not a search that cannot find anything.

const (
	// Neither has a shape the redactor's patterns know, so what masks them in
	// the database is the registration of the resolved value, as for any
	// credential, and nothing else. Plain words, so a secret scanner does not
	// take a test value for a real one.
	canaryCredToken = "canary-for-the-cred-credential"
	canaryAppToken  = "canary-for-the-app-credential"
	canaryGitUser   = "x-access-token"
)

// scanTool walks the given directories at the moment the run calls it and
// reports which files hold a canary, how many files it saw, and the request
// counts of the two git servers.
type scanTool struct {
	roots    []string
	needles  []string
	repos    []*gitsourcetest.Repo
	mu       sync.Mutex
	requests [][]int64 // per call, each repository's request count
}

func (s *scanTool) Name() string { return "Scan" }
func (s *scanTool) Description() string {
	return "test: look for the canary in the files under the volumes"
}
func (s *scanTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (s *scanTool) Execute(_ context.Context, _ json.RawMessage) (tools.Result, error) {
	files, found := scanFiles(s.roots, s.needles)
	var counts []int64
	for _, r := range s.repos {
		counts = append(counts, r.Requests.Load())
	}
	s.mu.Lock()
	s.requests = append(s.requests, counts)
	s.mu.Unlock()
	out, _ := json.Marshal(map[string]any{"files": files, "found": found})
	return tools.Result{Text: string(out)}, nil
}

// scanFiles reads every regular file under roots and returns how many it read
// and the ones holding a needle. Links are not followed: what a link points
// at is either under a root already or not the run's to have written.
func scanFiles(roots, needles []string) (int, []string) {
	n := 0
	var found []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			n++
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			for _, needle := range needles {
				if bytes.Contains(b, []byte(needle)) {
					found = append(found, p)
					break
				}
			}
			return nil
		})
	}
	return n, found
}

// canaryNeedles is each token as it could appear: itself, and inside the
// basic-auth header a clone sends.
func canaryNeedles(tokens ...string) []string {
	var out []string
	for _, tok := range tokens {
		out = append(out, tok, base64.StdEncoding.EncodeToString([]byte(canaryGitUser+":"+tok)))
	}
	return out
}

type canaryEnv struct {
	url      string
	st       interface{ Close() error }
	dbPath   string
	logs     *syncBuffer
	pool     string // the dynamic root: ephemeral volumes and clone leftovers
	work     string // the agent's own rw volume, where Bash runs
	scan     *scanTool
	repoCred *gitsourcetest.Repo
	repoApp  *gitsourcetest.Repo
	ghMints  *atomic.Int64
}

// newCanaryEnv builds a server whose tools are wired as cmd/loomcycle wires
// them: VolumeDef with a git source resolving credentials through the engine
// and minting App tokens through the minter, and Bash taking GITHUB_TOKEN from
// the credential store. restricted decides whether GITHUB_TOKEN carries
// allowed_urls (the git source needs it; Bash is then refused it).
func newCanaryEnv(t *testing.T, restricted bool, scripts func(e *canaryEnv) map[string]string) *canaryEnv {
	t.Helper()
	e := &canaryEnv{logs: &syncBuffer{}, ghMints: new(atomic.Int64)}
	log.SetOutput(io.MultiWriter(e.logs, os.Stderr))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	e.repoCred = gitsourcetest.New(t, gitsourcetest.Options{User: canaryGitUser, Token: canaryCredToken})
	e.repoApp = gitsourcetest.New(t, gitsourcetest.Options{User: canaryGitUser, Token: canaryAppToken})

	// A fake GitHub that mints the App token, for the one repository asked.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	gh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/app/installations/2/access_tokens" || !strings.Contains(string(body), `"repositories":["app"]`) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		e.ghMints.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, canaryAppToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(gh.Close)

	// The scripts name the servers, so they are written once those exist.
	js := scripts(e)
	codeRoot := t.TempDir()
	for agent, src := range js {
		if err := os.MkdirAll(filepath.Join(codeRoot, agent), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(codeRoot, agent, "index.js"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	realDir := func() string {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	e.pool, e.work = realDir(), realDir()

	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "code-js"
	cfg.Volumes = map[string]config.Volume{
		"pool": {Path: e.pool, Mode: "rw", DynamicRoot: true},
		"work": {Path: e.work, Mode: "rw"},
	}
	cfg.Agents = map[string]config.AgentDef{}
	for agent := range js {
		cfg.Agents[agent] = config.AgentDef{
			Provider: "code-js", Model: "code-js",
			Tools:           []string{"VolumeDef", "Bash", "Scan"},
			Volumes:         []string{"work"},
			VolumeDefScopes: []string{"any", builtin.GitSourceScope},
		}
	}

	e.dbPath = filepath.Join(t.TempDir(), "canary.db")
	st, err := storesqlite.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st

	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	engine := credential.NewEngine(st, sealer)
	put := func(name, value string, allowed []string) {
		t.Helper()
		var list *[]string
		if allowed != nil {
			norm, err := credential.NormalizeAllowedURLs(&allowed)
			if err != nil {
				t.Fatal(err)
			}
			list = norm
		}
		if _, err := engine.PutInline(context.Background(), credential.Identity{Scope: "tenant", Name: name}, value, nil, list); err != nil {
			t.Fatal(err)
		}
	}
	var credURLs []string
	if restricted {
		credURLs = []string{e.repoCred.URL}
	}
	put("GITHUB_TOKEN", canaryCredToken, credURLs)
	appCfg, _ := json.Marshal(map[string]string{"app_id": "1", "installation_id": "2", "private_key": keyPEM, "base_url": gh.URL})
	put("acme-app", string(appCfg), []string{e.repoApp.URL})

	var srv *Server
	register := func(v string) {
		if srv != nil {
			srv.Redactor().Register(v)
		}
	}
	minter := credential.NewGitHubAppMinter(nil, gh.Client())
	volumeDef := &builtin.VolumeDef{Cfg: cfg, Store: st, MaxNameLen: 64}
	volumeDef.Git = &builtin.GitSourceConfig{
		Hosts:    []string{e.repoCred.Host, e.repoApp.Host},
		MaxBytes: 16 << 20, MaxObjectBytes: 1 << 20, MaxFileBytes: 1 << 20, MaxFiles: 1000, MaxHistory: 64,
		Timeout:    30 * time.Second,
		Slots:      make(chan struct{}, 1),
		Transport:  e.repoCred.Transport, // both servers present httptest's one certificate
		Credential: builtin.GitCredentialResolver(engine, minter.TokenForRepository, register),
	}
	bash := &builtin.Bash{
		Enabled:      true,
		AllowedCreds: []string{"GITHUB_TOKEN"},
		CredResolve: func(ctx context.Context, name string) (string, bool) {
			ri := tools.RunIdentity(ctx)
			res, found, err := engine.Resolve(ctx, ri.TenantID, tools.AgentName(ctx), ri.UserID, name)
			if err != nil || !found {
				return "", false
			}
			register(res.Value)
			return res.Value, true
		},
	}
	e.scan = &scanTool{
		roots:   []string{e.pool, e.work},
		needles: canaryNeedles(canaryCredToken, canaryAppToken),
		repos:   []*gitsourcetest.Repo{e.repoCred, e.repoApp},
	}
	prov := codejs.New(codejs.Config{CodeRoot: codeRoot, RunTimeout: 60 * time.Second})
	srv = New(cfg, &stubResolver{p: prov}, []tools.Tool{volumeDef, bash, e.scan}, concurrency.New(4, 4, 5*time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	e.url = ts.URL
	return e
}

// run starts the agent and returns the live stream as the client read it.
func (e *canaryEnv) run(t *testing.T, agent string) string {
	t.Helper()
	resp, err := http.Post(e.url+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"`+agent+`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"review"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	return string(body)
}

// stored is everything the database holds on disk, the write-ahead log with it.
func (e *canaryEnv) stored(t *testing.T) string {
	t.Helper()
	var all []byte
	for _, p := range []string{e.dbPath, e.dbPath + "-wal"} {
		if b, err := os.ReadFile(p); err == nil {
			all = append(all, b...)
		}
	}
	if len(all) == 0 {
		t.Fatal("read no database bytes")
	}
	return string(all)
}

// where lists the places that hold a needle.
func where(needles []string, places map[string]string) []string {
	var out []string
	for name, text := range places {
		for _, n := range needles {
			if strings.Contains(text, n) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

func TestGitSourceCanary_TheTokenIsNowhere(t *testing.T) {
	e := newCanaryEnv(t, true, func(e *canaryEnv) map[string]string {
		return map[string]string{"reviewer": fmt.Sprintf(`
function run(input) {
  var a = VolumeDef({op: "create", name: "repo", ephemeral: true, mode: "ro",
    source: {git: {url: %q, commit: %q, credential: "$cred:GITHUB_TOKEN", diff_base: %q}}});
  var b = VolumeDef({op: "create", name: "app", ephemeral: true, mode: "ro",
    source: {git: {url: %q, commit: %q, credential: "$ghapp:acme-app"}}});
  var s;
  for (var i = 0; i < 10; i++) { s = Scan({call: i}); } // distinct, or the loop's repeat guard stops them
  var env = Bash({command: "env"});
  return {final_text: "clones " + JSON.stringify([a, b]) + " scan " + JSON.stringify(s) + " env " + JSON.stringify(env)};
}`, e.repoCred.URL, e.repoCred.Head, e.repoCred.Old, e.repoApp.URL, e.repoApp.Head)}
	})

	stream := e.run(t, "reviewer")
	if !strings.Contains(stream, `\"files\"`) || !strings.Contains(stream, "PATH=") {
		t.Fatalf("the run did not reach its scan and its Bash call:\n%s", stream)
	}

	// Not vacuous: both clones happened, through the minter for the App, and
	// the scan inside the run saw the volumes' files.
	if e.repoCred.Requests.Load() == 0 || e.repoApp.Requests.Load() == 0 {
		t.Fatalf("a repository was never fetched: cred=%d app=%d", e.repoCred.Requests.Load(), e.repoApp.Requests.Load())
	}
	if e.ghMints.Load() != 1 {
		t.Errorf("the App token was minted %d times, want 1", e.ghMints.Load())
	}
	e.scan.mu.Lock()
	calls := append([][]int64(nil), e.scan.requests...)
	e.scan.mu.Unlock()
	if len(calls) != 10 {
		t.Fatalf("Scan ran %d times, want 10", len(calls))
	}

	// The replay of a code-js run reuses each recorded result: the clones ran
	// once, before the first scan, and no tool call after them fetched again.
	final := []int64{e.repoCred.Requests.Load(), e.repoApp.Requests.Load()}
	for i, c := range calls {
		if c[0] != final[0] || c[1] != final[1] {
			t.Errorf("scan %d saw %v requests at the servers, the run ended at %v: a replay fetched again", i, c, final)
		}
	}

	needles := canaryNeedles(canaryCredToken, canaryAppToken)
	files, inFiles := scanFiles([]string{e.pool, e.work}, needles)
	places := map[string]string{
		"the live stream": stream,
		"the database":    e.stored(t),
		"the process log": e.logs.String(),
	}
	if got := where(needles, places); len(got) > 0 {
		t.Errorf("a token is in %v", got)
	}
	if strings.Contains(stream, `\"found\":[\"`) {
		t.Errorf("the scan inside the run found a token in a file: %s", stream)
	}
	if len(inFiles) > 0 {
		t.Errorf("after the run, a token is in %v (of %d files)", inFiles, files)
	}
}

// The control. The token is handed to Bash, as an unrestricted credential,
// and the model clones with it as a team did before the git source. The same
// search finds it: in the clone's .git/config, and in the stream that carried
// the Bash call's output.
func TestGitSourceCanary_ACloneThroughBashIsFound(t *testing.T) {
	e := newCanaryEnv(t, false, func(e *canaryEnv) map[string]string {
		clone := fmt.Sprintf("HOME=$PWD GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 git -c http.sslVerify=false clone -q https://%s:$GITHUB_TOKEN@%s/acme/app.git cloned && env",
			canaryGitUser, e.repoCred.Host)
		return map[string]string{"control": fmt.Sprintf(`
function run(input) {
  var out = Bash({command: %q});
  var s = Scan({});
  return {final_text: "bash " + JSON.stringify(out) + " scan " + JSON.stringify(s)};
}`, clone)}
	})
	stream := e.run(t, "control")

	needles := canaryNeedles(canaryCredToken)
	_, inFiles := scanFiles([]string{e.pool, e.work}, needles)
	if len(inFiles) == 0 {
		t.Errorf("a clone through Bash left no token in a file; the search cannot be trusted:\n%s", stream)
	}
	if got := where(needles, map[string]string{"the live stream": stream}); len(got) == 0 {
		t.Errorf("the token Bash printed is not in the live stream; the search cannot be trusted:\n%s", stream)
	}
}
