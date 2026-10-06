package http

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A detached walk keeps its starter's root run id, so its members use the
// starter tree's ephemeral volumes — which the tree's top-level run tore down
// when it ended, with the walk still running on them.

// volumeWriterProvider is a heldMemberProvider whose call, once released,
// writes to the ephemeral volume `work` through the real Write tool on the
// member's own ctx, and records what the write answered.
type volumeWriterProvider struct {
	*heldMemberProvider

	mu     sync.Mutex
	writes []tools.Result
}

func (p *volumeWriterProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	ch, err := p.heldMemberProvider.Call(ctx, req)
	if err != nil {
		return nil, err
	}
	res, _ := (&builtin.Write{}).Execute(ctx, json.RawMessage(`{"path":"note.txt","content":"from the walk","volume":"work"}`))
	p.mu.Lock()
	p.writes = append(p.writes, res)
	p.mu.Unlock()
	return ch, nil
}

func (p *volumeWriterProvider) results() []tools.Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tools.Result(nil), p.writes...)
}

// newVolumeDetachHarness is newDetachHarness with a dynamic root to hold
// ephemeral volumes and a worker that writes to one. It returns the root.
func newVolumeDetachHarness(t *testing.T) (*detachHarness, *volumeWriterProvider, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"worker": {Model: "stub-model", SystemPrompt: "work"}},
		Volumes:     map[string]config.Volume{"pool": {Path: root, Mode: "rw", DynamicRoot: true}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "detach_volumes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	prov := &volumeWriterProvider{heldMemberProvider: &heldMemberProvider{up: make(chan struct{}, 8), release: make(chan struct{})}}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	seedTenantTeam(t, st, "acme", "solo", soloTeam)
	var once sync.Once
	h := &detachHarness{t: t, srv: srv, st: st, prov: prov.heldMemberProvider,
		release: func() { once.Do(func() { close(prov.release) }) }}
	// Registered after the store's Close, so it runs first.
	t.Cleanup(func() {
		h.release()
		cancelAllRuns(srv)
		for _, r := range h.walkRuns() {
			_, _, _ = srv.CancelTurn(context.Background(), r.ID, "test cleanup")
			h.waitEnded(r.ID)
		}
	})
	return h, prov, root
}

// withEphemeralWork gives the starter's tree an ephemeral volume `work` the way
// VolumeDef create does — its directory, its row, and its entry in the set the
// tree's runs resolve through — and returns its directory.
func withEphemeralWork(t *testing.T, h *detachHarness, root string, s *starter) string {
	t.Helper()
	dir := filepath.Join(root, "_ephemeral", s.runID, "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir, "mode": "rw"})
	if _, err := h.st.EphemeralVolumeCreate(context.Background(), store.EphemeralVolumeDefRow{
		RootRunID: s.runID, Name: "work", TenantID: "acme", Definition: body,
	}); err != nil {
		t.Fatal(err)
	}
	set := tools.NewEphemeralVolumeSet()
	set.Add("work", tools.EphemeralVolumeRef{Root: dir})
	s.ctx = tools.WithEphemeralVolumes(s.ctx, set)
	return dir
}

// endTopLevel ends the starter's run the way a top-level run ends.
func (h *detachHarness) endTopLevel(s starter) {
	h.t.Helper()
	meta := runStateMeta{RunID: s.runID, IsTopLevel: true, RootRunID: s.runID}
	h.srv.finishRunWithCancel(context.Background(), context.Background(), s.runID, loop.RunResult{}, nil, meta)
}

// treeHolds is the number of detached walks holding root's tree.
func treeHolds(srv *Server, root string) int {
	srv.runTrees.mu.Lock()
	defer srv.runTrees.mu.Unlock()
	return srv.runTrees.held[root]
}

// waitTreeHeldBelow waits until fewer than n walks hold root's tree: a walk
// releases its hold after its run is recorded as ended, so its status alone
// does not say the release has happened.
func waitTreeHeldBelow(t *testing.T, srv *Server, root string, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); treeHolds(srv, root) >= n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("holds on %s = %d, want fewer than %d", root, treeHolds(srv, root), n)
		}
	}
}

func assertVolumeKept(t *testing.T, st store.Store, root, dir, when string) {
	t.Helper()
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s: the ephemeral volume is gone (%v)", when, err)
	}
	if rows, _ := st.EphemeralVolumeListByRun(context.Background(), root); len(rows) != 1 {
		t.Fatalf("%s: ephemeral rows = %+v, want the volume's", when, rows)
	}
}

func waitVolumePurged(t *testing.T, st store.Store, root, dir string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		_, statErr := os.Stat(dir)
		rows, _ := st.EphemeralVolumeListByRun(context.Background(), root)
		if os.IsNotExist(statErr) && len(rows) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tree was not purged: stat %v, rows %+v", statErr, rows)
		}
	}
}

func TestTeamDefRunDetach_StartersEphemeralVolumeOutlivesItsTopLevelRunUntilTheWalkEnds(t *testing.T) {
	h, prov, root := newVolumeDetachHarness(t)
	s := h.startAgent(nil, false)
	dir := withEphemeralWork(t, h, root, &s)
	walkID := h.runDetached(s)
	h.memberUp(walkID)

	h.endTopLevel(s)
	assertVolumeKept(t, h.st, s.runID, dir, "after the top-level run ended with the walk running")

	h.release()
	if walk := h.waitEnded(walkID); walk.Status != store.RunCompleted {
		t.Fatalf("walk = %s (%s), want completed", walk.Status, walk.ErrorMsg)
	}
	writes := prov.results()
	if len(writes) != 1 || writes[0].IsError {
		t.Fatalf("the member's write to the starter's volume = %+v, want one that succeeded", writes)
	}
	waitVolumePurged(t, h.st, s.runID, dir)
}

func TestTeamDefRunDetach_EphemeralTreeIsPurgedWhenTheLastOfSeveralWalksEnds(t *testing.T) {
	h, _, root := newVolumeDetachHarness(t)
	s := h.startAgent(nil, false)
	dir := withEphemeralWork(t, h, root, &s)
	first := h.runDetached(s)
	h.memberUp(first)
	second := h.runDetached(s)
	h.memberUp(second)

	h.endTopLevel(s)
	assertVolumeKept(t, h.st, s.runID, dir, "after the top-level run ended with two walks running")
	if _, _, err := h.srv.CancelTurn(tenantOperatorCtx("acme"), first, "stop one"); err != nil {
		t.Fatalf("cancel the first walk: %v", err)
	}
	h.waitEnded(first)
	waitTreeHeldBelow(t, h.srv, s.runID, 2) // the first walk has released
	assertVolumeKept(t, h.st, s.runID, dir, "after the first of two walks ended")

	h.release()
	h.waitEnded(second)
	waitVolumePurged(t, h.st, s.runID, dir)
}

// The walk ending first must neither purge the tree its still-running root
// uses nor leave a hold behind that keeps the root's own end from purging it.
func TestTeamDefRunDetach_WalkEndingBeforeItsRootLeavesThePurgeToTheRoot(t *testing.T) {
	h, _, root := newVolumeDetachHarness(t)
	s := h.startAgent(nil, false)
	dir := withEphemeralWork(t, h, root, &s)
	walkID := h.runDetached(s)
	h.memberUp(walkID)

	h.release()
	h.waitEnded(walkID)
	waitTreeHeldBelow(t, h.srv, s.runID, 1)
	assertVolumeKept(t, h.st, s.runID, dir, "after the walk ended under a running root")

	h.endTopLevel(s)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the root's end did not purge its tree: stat %v", err)
	}
	if rows, _ := h.st.EphemeralVolumeListByRun(context.Background(), s.runID); len(rows) != 0 {
		t.Errorf("the root's end left ephemeral rows %+v", rows)
	}
}
