package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A paused run resumes on the AgentDef version it started on. These tests
// start a real run on v1, change what the agent's name resolves to while the
// run is paused, resume it, and check what the model is offered and what a
// tool call does — not what the configuration says.

// ranTool is a tool that records whether it ran.
type ranTool struct {
	name  string
	calls atomic.Int32
}

func (c *ranTool) Name() string                 { return c.name }
func (c *ranTool) Description() string          { return c.name }
func (c *ranTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (c *ranTool) Execute(context.Context, json.RawMessage) (tools.Result, error) {
	c.calls.Add(1)
	return tools.Result{Text: c.name + " ran"}, nil
}

// goneStore is a store whose AgentDef rows in gone have been deleted, and
// whose reads of the rows in faulty fail.
type goneStore struct {
	store.Store
	mu     sync.Mutex
	gone   map[string]bool
	faulty map[string]bool
}

func (g *goneStore) delete(defID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gone[defID] = true
}

func (g *goneStore) AgentDefGet(ctx context.Context, defID string) (store.AgentDefRow, error) {
	g.mu.Lock()
	gone, faulty := g.gone[defID], g.faulty[defID]
	g.mu.Unlock()
	if gone {
		return store.AgentDefRow{}, &store.ErrNotFound{Kind: "agent_def", ID: defID}
	}
	if faulty {
		return store.AgentDefRow{}, errors.New("database unavailable")
	}
	return g.Store.AgentDefGet(ctx, defID)
}

// versionFixture is a server whose "worker" is a runtime-authored agent, and a
// tool, Wider, that only a later version of it is given.
type versionFixture struct {
	srv   *Server
	st    *goneStore
	ts    *httptest.Server
	prov  *recordingScriptedProvider
	wider *ranTool
}

func newVersionFixture(t *testing.T, cfg *config.Config, scripts ...[]providers.Event) *versionFixture {
	t.Helper()
	sq, err := storesqlite.Open(filepath.Join(t.TempDir(), "agent_version.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	f := &versionFixture{
		st:    &goneStore{Store: sq, gone: map[string]bool{}, faulty: map[string]bool{}},
		prov:  &recordingScriptedProvider{scripts: scripts, defaultS: endTurn()},
		wider: &ranTool{name: "Wider"},
	}
	f.srv = New(cfg, &stubResolver{p: f.prov}, []tools.Tool{&ranTool{name: "Narrow"}, f.wider},
		concurrency.New(8, 8, 5*time.Second), f.st)
	f.ts = httptest.NewServer(f.srv.Mux())
	t.Cleanup(f.ts.Close)
	return f
}

func versionConfig() *config.Config {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	return cfg
}

func workerDef(prompt string, toolNames ...string) string {
	b, _ := json.Marshal(map[string]any{
		"provider": "scripted", "model": "stub-model", "system_prompt": prompt, "tools": toolNames,
	})
	return string(b)
}

func offered(req providers.Request) []string {
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func systemText(req providers.Request) string {
	var b strings.Builder
	for _, c := range req.System {
		b.WriteString(c.Text)
	}
	return b.String()
}

func hasTool(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// assertResumedOnV1 checks the resumed turn (request index i) and its tool
// call: v1's prompt and tools, and Wider — which only v2 has — refused.
func assertResumedOnV1(t *testing.T, f *versionFixture, i int) {
	t.Helper()
	reqs := f.prov.waitForRequests(t, i+1)
	resumed := reqs[i]
	if hasTool(offered(resumed), "Wider") {
		t.Errorf("the resumed run was offered %v — Wider is only in a version promoted while it was paused", offered(resumed))
	}
	if !hasTool(offered(resumed), "Narrow") {
		t.Errorf("the resumed run was offered %v, want v1's Narrow", offered(resumed))
	}
	if sys := systemText(resumed); !strings.Contains(sys, "prompt v1") || strings.Contains(sys, "prompt v2") {
		t.Errorf("the resumed run's system prompt = %q, want v1's", sys)
	}
	if n := f.wider.calls.Load(); n != 0 {
		t.Errorf("Wider ran %d time(s) in a run whose version does not grant it", n)
	}
}

func TestResumedRun_ContinuesOnTheAgentVersionItStartedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change is what happens to "worker" while the run is paused.
		change func(t *testing.T, st store.Store)
	}{
		{"a wider version promoted", func(t *testing.T, st store.Store) {
			putAgentDef(t, st, "def_worker_v2", "worker", 2, workerDef("prompt v2", "Narrow", "Wider"), true)
		}},
		{"a wider fork promoted", func(t *testing.T, st store.Store) {
			ctx := context.Background()
			if _, err := st.AgentDefCreate(ctx, store.AgentDefRow{
				DefID: "def_worker_v2", Name: "worker", Version: 2, ParentDefID: "def_worker_v1",
				Definition: []byte(workerDef("prompt v2", "Narrow", "Wider")), CreatedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.AgentDefSetActive(ctx, "", "worker", "def_worker_v2", ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"its version retired", func(t *testing.T, st store.Store) {
			putAgentDef(t, st, "def_worker_v2", "worker", 2, workerDef("prompt v2", "Narrow", "Wider"), true)
			if err := st.AgentDefSetRetired(context.Background(), "def_worker_v1", true); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Live: one turn that ends. Resumed: the model calls Wider, then ends.
			f := newVersionFixture(t, versionConfig(), endTurn(), toolCallTurn("tu_w", "Wider", `{}`), endTurn())
			putAgentDef(t, f.st, "def_worker_v1", "worker", 1, workerDef("prompt v1", "Narrow"), true)

			run := postAgentRun(t, f.st, f.ts, "worker")
			if live := offered(f.prov.waitForRequests(t, 1)[0]); hasTool(live, "Wider") || !hasTool(live, "Narrow") {
				t.Fatalf("fixture drifted: the live run on v1 was offered %v", live)
			}

			tc.change(t, f.st)
			resumeAndFinish(t, f.srv, run)
			assertResumedOnV1(t, f, 1)
		})
	}
}

// pausedOnV1 is the row a run paused on v1 leaves — still running, its record
// naming v1 (a live start writes that record, see
// TestRunStart_RecordsTheAgentVersionItResolvedTo) — with v2 active since.
func pausedOnV1(t *testing.T, f *versionFixture) store.Run {
	t.Helper()
	ctx := context.Background()
	putAgentDef(t, f.st, "def_worker_v1", "worker", 1, workerDef("prompt v1", "Narrow"), true)
	sess, err := f.st.CreateSession(ctx, "", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := runConfigRecord{AgentVersion: &agentVersionRecord{DefID: "def_worker_v1"}}
	run, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_paused_v1", UserID: "alice", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	putAgentDef(t, f.st, "def_worker_v2", "worker", 2, workerDef("prompt v2", "Narrow", "Wider"), true)
	parkForResume(t, f.srv, run.ID)
	return run
}

// Deleted, not retired: nothing to resume on, and the version active now is
// not a substitute — the run fails, saying why, and never runs.
func TestResumedRun_WhoseAgentVersionIsGoneFailsWithoutRunning(t *testing.T) {
	f := newVersionFixture(t, versionConfig())
	run := pausedOnV1(t, f)
	f.st.delete("def_worker_v1")

	ctx := context.Background()
	if n, warnings := f.srv.ResumePausedRuns(ctx); n != 0 || len(warnings) != 1 {
		t.Fatalf("ResumePausedRuns = %d, %v; want 0 re-dispatched and one warning", n, warnings)
	}
	got, err := f.st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunFailed || !strings.Contains(got.ErrorMsg, "the version it started on (def_worker_v1) no longer exists") {
		t.Errorf("run status %q, error %q; want failed because its version no longer exists", got.Status, got.ErrorMsg)
	}
	if n := len(f.prov.requests()); n != 0 {
		t.Errorf("the provider saw %d calls, want none — the run resumed on another version", n)
	}
}

// A store fault reading the version is not its deletion: the run neither runs
// nor is failed, and stays paused for the next resume to try again.
func TestResumedRun_WhoseAgentVersionCannotBeReadStaysPaused(t *testing.T) {
	f := newVersionFixture(t, versionConfig())
	run := pausedOnV1(t, f)
	f.st.mu.Lock()
	f.st.faulty["def_worker_v1"] = true
	f.st.mu.Unlock()

	ctx := context.Background()
	if n, warnings := f.srv.ResumePausedRuns(ctx); n != 0 || len(warnings) != 1 {
		t.Fatalf("ResumePausedRuns = %d, %v; want 0 re-dispatched and one warning", n, warnings)
	}
	got, err := f.st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.RunRunning || got.PauseState != store.PauseStatePaused {
		t.Errorf("run status %q pause_state %q (%s); want still paused", got.Status, got.PauseState, got.ErrorMsg)
	}
	if n := len(f.prov.requests()); n != 0 {
		t.Errorf("the provider saw %d calls, want none", n)
	}
}

// The operator's yaml has no versions: a run of a yaml agent resumes by name.
func TestResumedRun_OfAYamlAgentResumesByName(t *testing.T) {
	cfg := versionConfig()
	cfg.Agents = map[string]config.AgentDef{
		"yaml-worker": {Model: "stub-model", Tools: []string{"Narrow"}, SystemPrompt: "prompt v1"},
	}
	f := newVersionFixture(t, cfg, endTurn(), toolCallTurn("tu_n", "Narrow", `{}`), endTurn())
	run := postAgentRun(t, f.st, f.ts, "yaml-worker")
	f.prov.waitForRequests(t, 1)

	resumeAndFinish(t, f.srv, run)
	assertResumedOnV1(t, f, 1)
	if got, _ := f.st.GetRun(context.Background(), run.ID); got.Status != store.RunCompleted {
		t.Errorf("the resumed yaml run ended %q (%s), want completed", got.Status, got.ErrorMsg)
	}
}

// A run that started on a definition with no versions, whose name a tenant's
// AgentDef has shadowed since, did not start on that AgentDef: it fails
// rather than resuming on it.
func TestResumedRun_WhoseYamlAgentWasShadowedFails(t *testing.T) {
	cfg := versionConfig()
	cfg.Agents = map[string]config.AgentDef{
		"worker": {Model: "stub-model", Tools: []string{"Narrow"}, SystemPrompt: "prompt v1"},
	}
	f := newVersionFixture(t, cfg)
	ctx := context.Background()
	sess, err := f.st.CreateSession(ctx, "acme", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := runConfigRecord{AgentVersion: &agentVersionRecord{}} // started on the yaml
	run, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_shadowed", UserID: "alice", TenantID: "acme", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_acme_worker", Name: "worker", Version: 1, TenantID: "acme",
		Definition: []byte(workerDef("prompt v2", "Narrow", "Wider")), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AgentDefSetActive(ctx, "acme", "worker", "def_acme_worker", ""); err != nil {
		t.Fatal(err)
	}
	parkForResume(t, f.srv, run.ID)

	if n, _ := f.srv.ResumePausedRuns(ctx); n != 0 {
		t.Fatalf("re-dispatched %d; a shadowed run must not resume on the tenant's definition", n)
	}
	got, _ := f.st.GetRun(ctx, run.ID)
	if got.Status != store.RunFailed || !strings.Contains(got.ErrorMsg, "not the definition the run started on") {
		t.Errorf("run status %q, error %q; want failed as shadowed", got.Status, got.ErrorMsg)
	}
	if n := len(f.prov.requests()); n != 0 {
		t.Errorf("the provider saw %d calls, want none", n)
	}
}

// A child its parent pinned by def_id resumes on that pinned version laid over
// the base its name resolved to at spawn — not over, or instead of, the
// version active now. The pinned version is retired too, which a resume
// accepts as it does for any version a run started on.
func TestResumedChild_PinnedByDefIDKeepsItsPinnedVersion(t *testing.T) {
	cfg := versionConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "role:parent."},
	}
	// Sequential: parent spawns → child's live turn → parent ends → (resume)
	// child calls Wider → child ends.
	f := newVersionFixture(t, cfg,
		toolCallTurn("tu_spawn", "Agent", `{"name":"worker","prompt":"hi","def_id":"def_worker_pin"}`),
		endTurn(), endTurn(),
		toolCallTurn("tu_w", "Wider", `{}`), endTurn())
	putAgentDef(t, f.st, "def_worker_v1", "worker", 1, workerDef("prompt v0", "Narrow"), true)
	putAgentDef(t, f.st, "def_worker_pin", "worker", 2, workerDef("prompt v1", "Narrow"), false)

	child := spawnLiveChild(t, f.st, f.ts, "a_pin_parent", "")
	if live := systemText(f.prov.waitForRequests(t, 3)[1]); !strings.Contains(live, "prompt v1") {
		t.Fatalf("fixture drifted: the live child ran on %q, not its pinned version", live)
	}

	putAgentDef(t, f.st, "def_worker_v3", "worker", 3, workerDef("prompt v2", "Narrow", "Wider"), true)
	if err := f.st.AgentDefSetRetired(context.Background(), "def_worker_pin", true); err != nil {
		t.Fatal(err)
	}
	resumeAndFinish(t, f.srv, child)
	assertResumedOnV1(t, f, 3)
}

// A recorded version must be one the run's name could have resolved to: its
// agent's, in its tenant or the shared one. A record naming another tenant's
// (a record restored from elsewhere, say) does not resume on it.
func TestResumedRun_WhoseRecordedVersionIsAnotherTenantsFails(t *testing.T) {
	f := newVersionFixture(t, versionConfig())
	ctx := context.Background()
	// The name resolves in acme (to the shared version), so a resume by name
	// would run; only the record decides here.
	putAgentDef(t, f.st, "def_worker_v1", "worker", 1, workerDef("prompt v1", "Narrow"), true)
	if _, err := f.st.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "def_other_worker", Name: "worker", Version: 1, TenantID: "other",
		Definition: []byte(workerDef("prompt v2", "Narrow", "Wider")), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := f.st.CreateSession(ctx, "acme", "worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	rec := runConfigRecord{AgentVersion: &agentVersionRecord{DefID: "def_other_worker"}}
	run, err := f.st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_cross", UserID: "alice", TenantID: "acme", Model: "stub-model", RunConfig: rec.marshal(),
	})
	if err != nil {
		t.Fatal(err)
	}
	parkForResume(t, f.srv, run.ID)

	if n, _ := f.srv.ResumePausedRuns(ctx); n != 0 {
		t.Fatalf("re-dispatched %d on another tenant's version", n)
	}
	if got, _ := f.st.GetRun(ctx, run.ID); got.Status != store.RunFailed {
		t.Errorf("run status %q (%s), want failed", got.Status, got.ErrorMsg)
	}
	if n := len(f.prov.requests()); n != 0 {
		t.Errorf("the provider saw %d calls, want none", n)
	}
}
