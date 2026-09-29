package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A paused sub-run is resumed with no parent run to inherit from, so what it
// knows about its parent has to come from its row. These tests pin each thing a
// live sub-run knows: the parent run its hooks report, the spawn tree its usage
// rolls up to and whose state it shares, and the parent whose cancel stops it.

// resumeTreeConfig is a config whose "resumer" agent reports its agent_stop and
// run_end to hookURL, with a dynamic_root volume under volRoot.
func resumeTreeConfig(provider, hookURL, volRoot string) *config.Config {
	def := config.AgentDef{Provider: provider, Model: "stub-model", SystemPrompt: "resume"}
	if hookURL != "" {
		def.Hooks = hooks.EventHooks{
			hooks.PhaseAgentStop: {{Inline: &hooks.Inline{Name: "stop", URL: hookURL}}},
			hooks.PhaseRunEnd:    {{Inline: &hooks.Inline{Name: "end", URL: hookURL}}},
		}
	}
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: provider, Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"resumer": def},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	if volRoot != "" {
		cfg.Volumes = map[string]config.Volume{"pool": {Path: volRoot, Mode: "rw", DynamicRoot: true}}
	}
	cfg.Env.AuthToken = ""
	return cfg
}

// answeringProvider ends every turn at once, reporting the tokens it spent.
func answeringProvider() *scriptedProvider {
	return &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "resumed"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 10, OutputTokens: 5, Model: "stub-model"}},
	}}
}

// createTreeRun files a run of the resumer agent under its own session.
func createTreeRun(t *testing.T, srv *Server, ident store.RunIdentity) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, ident.TenantID, "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	ident.UserID, ident.Model = "alice", "stub-model"
	run, err := srv.store.CreateRun(ctx, sess.ID, ident)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// pauseMidTurn gives run a pending prompt and parks it as a snapshot would.
func pauseMidTurn(t *testing.T, srv *Server, run store.Run) {
	t.Helper()
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go on"}}},
	})
	if err := srv.store.SetRunPauseState(context.Background(), run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
}

func resumeOne(t *testing.T, srv *Server) {
	t.Helper()
	if n, warnings := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
}

// hookBody returns the payload of the given phase for runID.
func hookBody(t *testing.T, rec *recordingHook, phase hooks.Phase, runID string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		for _, b := range rec.bodies {
			if strings.Contains(b, `"phase":"`+string(phase)+`"`) && strings.Contains(b, `"run_id":"`+runID+`"`) {
				rec.mu.Unlock()
				return b
			}
		}
		rec.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s hook payload for run %s", phase, runID)
	return ""
}

// agent_stop reads the parent from the loop's context, run_end from the run's
// state record: both are the crossings a resume rebuilds.
func TestResumePausedRuns_AResumedChildsHooksNameItsParentRun(t *testing.T) {
	rec := newRecordingHook(t, `{}`)
	srv, _ := makeServer(t, answeringProvider(), resumeTreeConfig("scripted", rec.srv.URL, ""))
	parent := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_parent"})
	child := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_child", ParentAgentID: "a_parent", ParentRunID: parent.ID})
	pauseMidTurn(t, srv, child)

	resumeOne(t, srv)
	waitRunStatus(t, srv.store, child.ID, store.RunCompleted)

	want := `"parent_run_id":"` + parent.ID + `"`
	for _, phase := range []hooks.Phase{hooks.PhaseAgentStop, hooks.PhaseRunEnd} {
		if body := hookBody(t, rec, phase, child.ID); !strings.Contains(body, want) {
			t.Errorf("resumed child's %s payload lacks %s: %s", phase, want, body)
		}
	}
}

func TestResumePausedRuns_AResumedTopLevelRunsHooksNameNoParent(t *testing.T) {
	rec := newRecordingHook(t, `{}`)
	srv, _ := makeServer(t, answeringProvider(), resumeTreeConfig("scripted", rec.srv.URL, ""))
	top := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_top"})
	pauseMidTurn(t, srv, top)

	resumeOne(t, srv)
	waitRunStatus(t, srv.store, top.ID, store.RunCompleted)

	for _, phase := range []hooks.Phase{hooks.PhaseAgentStop, hooks.PhaseRunEnd} {
		if body := hookBody(t, rec, phase, top.ID); strings.Contains(body, "parent_run_id") {
			t.Errorf("resumed top-level run's %s payload names a parent: %s", phase, body)
		}
	}
}

// Two levels deep, so the root is found by walking and not read off the parent.
func TestResumePausedRuns_AResumedGrandchildsUsageRollsUpToItsTreesRoot(t *testing.T) {
	srv, _ := makeServer(t, answeringProvider(), resumeTreeConfig("scripted", "", ""))
	root := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_root"})
	mid := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_mid", ParentAgentID: "a_root", ParentRunID: root.ID})
	leaf := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_leaf", ParentAgentID: "a_mid", ParentRunID: mid.ID})
	pauseMidTurn(t, srv, leaf)

	resumeOne(t, srv)
	waitRunStatus(t, srv.store, leaf.ID, store.RunCompleted)

	rows, err := srv.store.TokenUsageForRun(context.Background(), leaf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("the resumed grandchild recorded no usage")
	}
	for _, r := range rows {
		if r.ParentRunID != root.ID {
			t.Errorf("resumed grandchild's usage rolls up to %q, want the tree's root %q", r.ParentRunID, root.ID)
		}
	}
}

// The tree's ephemeral volumes are the root's to tear down. A resumed child
// that now knows the root must not also act as it.
func TestResumePausedRuns_AResumedChildLeavesItsTreesVolumesInPlace(t *testing.T) {
	volRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := makeServer(t, answeringProvider(), resumeTreeConfig("scripted", "", volRoot))
	parent := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_parent"})
	child := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_child", ParentAgentID: "a_parent", ParentRunID: parent.ID})
	dir := filepath.Join(volRoot, "_ephemeral", parent.ID, "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir, "mode": "rw"})
	if _, err := srv.store.EphemeralVolumeCreate(context.Background(), store.EphemeralVolumeDefRow{RootRunID: parent.ID, Name: "work", Definition: body}); err != nil {
		t.Fatal(err)
	}
	pauseMidTurn(t, srv, child)

	resumeOne(t, srv)
	waitRunStatus(t, srv.store, child.ID, store.RunCompleted)
	// A purge runs as the run finishes, before its goroutine leaves the cancel
	// registry: once it has left, a purge would have happened.
	waitFor(t, "the resumed child to leave the cancel registry", func() bool {
		_, live := srv.cancelReg.Get("a_child")
		return !live
	})

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("the tree's ephemeral dir was removed when a resumed child finished: err=%v", err)
	}
	if rows, _ := srv.store.EphemeralVolumeListByRun(context.Background(), parent.ID); len(rows) != 1 {
		t.Errorf("the tree's ephemeral rows were deleted when a resumed child finished: %+v", rows)
	}
}

// blockingProvider holds its turn open until the run is cancelled, and says
// when a turn has begun.
type blockingProvider struct{ started chan struct{} }

func (b *blockingProvider) ID() string                    { return "stub" }
func (b *blockingProvider) Probe(_ context.Context) error { return nil }
func (b *blockingProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (b *blockingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (b *blockingProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	ch := make(chan providers.Event)
	select {
	case b.started <- struct{}{}:
	default:
	}
	go func() {
		defer close(ch)
		<-ctx.Done()
	}()
	return ch, nil
}

// A live child is stopped by its parent's context. A resumed one runs under a
// context of its own, so its parent's cancel reaches it only through the
// registry's cascade over parent agent ids.
func TestResumePausedRuns_CancellingTheParentCancelsAResumedChild(t *testing.T) {
	prov := &blockingProvider{started: make(chan struct{}, 1)}
	srv, _ := makeServer(t, prov, resumeTreeConfig("stub", "", ""))
	parent := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_parent"})
	child := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_child", ParentAgentID: "a_parent", ParentRunID: parent.ID})
	pauseMidTurn(t, srv, child)
	// The parent, live on this replica.
	if err := srv.cancelReg.Register(cancel.Entry{AgentID: "a_parent", RunID: parent.ID, SessionID: parent.SessionID, UserID: "alice", StartedAt: time.Now()}, func(error) {}); err != nil {
		t.Fatal(err)
	}
	// Whatever the outcome, do not leave the child's turn blocked past the test.
	t.Cleanup(func() { srv.cancelReg.Cancel("a_child", "test cleanup") })

	resumeOne(t, srv)
	select {
	case <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the resumed child never started its turn")
	}
	if _, ok := srv.cancelReg.Cancel("a_parent", "operator stop"); !ok {
		t.Fatal("the parent was not registered")
	}

	got := waitRunStatus(t, srv.store, child.ID, store.RunCancelled)
	if got.StopReason != "operator stop" {
		t.Errorf("resumed child's stop reason = %q, want the parent's cancel reason", got.StopReason)
	}
}

// The walk to the root, including every way it can fail to find one.
func TestResumedRootRunID_WalksToTheTopOrFallsBackToTheRunItself(t *testing.T) {
	rows := map[string]store.Run{
		"r_root":  {ID: "r_root", TenantID: "acme"},
		"r_mid":   {ID: "r_mid", TenantID: "acme", ParentRunID: "r_root"},
		"r_leaf":  {ID: "r_leaf", TenantID: "acme", ParentRunID: "r_mid"},
		"r_loopA": {ID: "r_loopA", TenantID: "acme", ParentRunID: "r_loopB"},
		"r_loopB": {ID: "r_loopB", TenantID: "acme", ParentRunID: "r_loopA"},
		"r_other": {ID: "r_other", TenantID: "globex"},
		"r_empty": {TenantID: "acme"}, // no id: a row that is not the one asked for
	}
	reads := 0
	getRun := func(_ context.Context, id string) (store.Run, error) {
		reads++
		if r, ok := rows[id]; ok {
			return r, nil
		}
		return store.Run{}, errors.New("not found")
	}
	for _, tc := range []struct {
		name    string
		run     store.Run
		want    string
		wantErr bool
		// maxReads, when set, is how far the walk may go before it stops.
		maxReads int
	}{
		{"top-level run is its own root", rows["r_root"], "r_root", false, 0},
		{"grandchild reaches the root", rows["r_leaf"], "r_root", false, 0},
		{"unrecorded parent run", store.Run{ID: "r_old", TenantID: "acme"}, "r_old", false, 0},
		{"missing ancestor", store.Run{ID: "r_orphan", TenantID: "acme", ParentRunID: "r_gone"}, "r_orphan", true, 0},
		{"ancestor read back empty", store.Run{ID: "r_blank", TenantID: "acme", ParentRunID: "r_empty"}, "r_blank", true, 0},
		{"cycle", store.Run{ID: "r_in", TenantID: "acme", ParentRunID: "r_loopA"}, "r_in", true, 2},
		{"ancestor in another tenant", store.Run{ID: "r_x", TenantID: "acme", ParentRunID: "r_other"}, "r_x", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads = 0
			got, err := resumedRootRunID(context.Background(), getRun, tc.run)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("root = %q, err = %v; want %q, err=%v", got, err, tc.want, tc.wantErr)
			}
			if tc.maxReads > 0 && reads > tc.maxReads {
				t.Errorf("walk read %d runs, want at most %d", reads, tc.maxReads)
			}
		})
	}

	// A chain longer than the bound stops rather than walking it all.
	deep := map[string]store.Run{}
	for i := 0; i <= maxResumeAncestry+1; i++ {
		deep[fmt.Sprint(i)] = store.Run{ID: fmt.Sprint(i), ParentRunID: fmt.Sprint(i + 1)}
	}
	calls := 0
	got, err := resumedRootRunID(context.Background(), func(_ context.Context, id string) (store.Run, error) {
		calls++
		return deep[id], nil
	}, deep["0"])
	if got != "0" || err == nil || calls > maxResumeAncestry {
		t.Errorf("deep chain: root = %q, err = %v, reads = %d; want own id, an error, at most %d reads", got, err, calls, maxResumeAncestry)
	}
}
