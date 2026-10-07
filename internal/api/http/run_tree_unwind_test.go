package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run's background children are cancelled when it ends, not waited for, and
// a cancelled child stops only at its next await. The tree's ephemeral state
// must outlive a child still inside a tool call, and be torn down once the
// child's goroutine has returned.

// hangTool is a tool call in flight: it says when it has begun, under which
// tree, and returns only when gate is closed — whatever its ctx says.
type hangTool struct {
	entered chan struct{}
	once    sync.Once
	gate    chan struct{}

	mu   sync.Mutex
	root string
}

func newHangTool() *hangTool {
	return &hangTool{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (h *hangTool) Name() string                 { return "Hang" }
func (h *hangTool) Description() string          { return "hangs until released" }
func (h *hangTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (h *hangTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	h.mu.Lock()
	h.root = tools.RunIdentity(ctx).RootRunID
	h.mu.Unlock()
	h.once.Do(func() { close(h.entered) })
	<-h.gate
	return tools.Result{Text: "released"}, nil
}

// waitEntered waits for the tool call to begin and returns the tree it runs in.
func (h *hangTool) waitEntered(t *testing.T) string {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the child never entered its tool call")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.root
}

// unwindProvider scripts the lead, whose every call after its first waits for
// proceed, and has each child call Hang once and then answer.
type unwindProvider struct {
	mu      sync.Mutex
	parent  [][]providers.Event
	calls   int
	proceed chan struct{}
}

func (p *unwindProvider) ID() string                  { return "stub" }
func (p *unwindProvider) Probe(context.Context) error { return nil }
func (p *unwindProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *unwindProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *unwindProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	var events []providers.Event
	if len(req.System) > 0 && strings.Contains(req.System[0].Text, "you are a child") {
		events = []providers.Event{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_hang", Name: "Hang", Input: json.RawMessage(`{}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		}
		if hasToolResult(req.Messages) {
			events = answer("child done")
		}
	} else {
		p.mu.Lock()
		i := p.calls
		p.calls++
		p.mu.Unlock()
		if i > 0 {
			select {
			case <-p.proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if i >= len(p.parent) {
			return nil, context.Canceled
		}
		events = p.parent[i]
	}
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func hasToolResult(msgs []providers.Message) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

// unwindServer is a server whose lead may start children and whose worker
// calls hang, with a dynamic root for ephemeral volumes. It returns the root.
func unwindServer(t *testing.T, prov providers.Provider, hang *hangTool) (*Server, store.Store, string) {
	t.Helper()
	volRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"lead":   {Model: "stub-model", Tools: []string{"Agent"}, SystemPrompt: "you are the lead"},
		"worker": {Model: "stub-model", Tools: []string{"Hang"}, SystemPrompt: "you are a child"},
	}
	cfg.Volumes = map[string]config.Volume{"pool": {Path: volRoot, Mode: "rw", DynamicRoot: true}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "unwind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{hang}, concurrency.New(8, 8, time.Second), st)
	return srv, st, volRoot
}

// seedTreeVolume gives the tree rooted at root an ephemeral volume — its
// directory and its row — and returns the directory.
func seedTreeVolume(t *testing.T, st store.Store, volRoot, root string) string {
	t.Helper()
	dir := filepath.Join(volRoot, "_ephemeral", root, "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"path": dir, "mode": "rw"})
	if _, err := st.EphemeralVolumeCreate(context.Background(), store.EphemeralVolumeDefRow{
		RootRunID: root, Name: "work", Definition: body,
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// waitTopLevelEnded waits for the lead's run to be over: its stream closed and
// its agent gone from the cancel registry. The purge of its tree runs before
// either, so by now it has happened unless something holds the tree.
func waitTopLevelEnded(t *testing.T, srv *Server, body <-chan string, root string) {
	t.Helper()
	select {
	case <-body:
	case <-time.After(10 * time.Second):
		t.Fatal("the top-level run did not end")
	}
	run, err := srv.store.GetRun(context.Background(), root)
	if err != nil || run.Status != store.RunCompleted {
		t.Fatalf("top-level run = %+v, %v; want completed", run, err)
	}
	waitFor(t, "the top-level run to leave the cancel registry", func() bool {
		for _, e := range srv.cancelReg.ListAll() {
			if e.RunID == root {
				return false
			}
		}
		return true
	})
}

// A poll-mode child cancelled by its parent's end while inside a tool call
// keeps the tree alive until it has unwound, then the tree is purged.
func TestRunTree_PurgeWaitsForAPollChildUnwindingInsideAToolCall(t *testing.T) {
	hang := newHangTool()
	prov := &unwindProvider{proceed: make(chan struct{}), parent: [][]providers.Event{
		agentCall("tu_1", `{"op":"spawn","name":"worker","prompt":"one","mode":"poll","on_parent_end":"cancel"}`),
		answer("not waiting"),
	}}
	srv, st, volRoot := unwindServer(t, prov, hang)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hang.gate) }) }
	defer release()

	body := postLead(t, ts)
	root := hang.waitEntered(t)
	if root == "" {
		t.Fatal("the child's tool call ran in no tree")
	}
	dir := seedTreeVolume(t, st, volRoot, root)
	close(prov.proceed) // the lead ends its turn; its end cancels the child

	waitTopLevelEnded(t, srv, body, root)
	assertVolumeKept(t, st, root, dir, "after the top-level run ended with its child inside a tool call")
	if n := treeHolds(srv, root); n != 1 {
		t.Errorf("holds on the tree = %d, want the child's one", n)
	}

	release()
	waitVolumePurged(t, st, root, dir)
	waitTreeHeldBelow(t, srv, root, 1)
}

// A resident child closed by its parent's end while inside a tool call keeps
// the tree alive until it has unwound, then the tree is purged.
func TestRunTree_PurgeWaitsForAResidentChildUnwindingInsideAToolCall(t *testing.T) {
	hang := newHangTool()
	prov := &unwindProvider{proceed: make(chan struct{}), parent: [][]providers.Event{
		agentCall("tu_1", `{"op":"open","name":"worker","prompt":"one","timeout_ms":1}`),
		answer("leaving it open"),
	}}
	srv, st, volRoot := unwindServer(t, prov, hang)
	srv.SetSteerRegistry(steer.NewRegistry(0)) // a resident child parks on it between turns
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hang.gate) }) }
	defer release()

	body := postLead(t, ts)
	root := hang.waitEntered(t)
	dir := seedTreeVolume(t, st, volRoot, root)
	close(prov.proceed)

	waitTopLevelEnded(t, srv, body, root)
	assertVolumeKept(t, st, root, dir, "after the top-level run ended with its resident child inside a tool call")

	release()
	waitVolumePurged(t, st, root, dir)
	waitTreeHeldBelow(t, srv, root, 1)
}

// With no child outliving it, the top-level run's end purges its tree at once
// and leaves no hold behind.
func TestRunTree_TopLevelRunWithNoChildrenPurgesAtItsEnd(t *testing.T) {
	prov := &unwindProvider{proceed: make(chan struct{}), parent: [][]providers.Event{
		// The first call is answered at once; a second is what waits, so the
		// run is given a tool turn first.
		{
			{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Agent", Input: json.RawMessage(`{"op":"poll"}`)}},
			{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
		},
		answer("done"),
	}}
	srv, st, volRoot := unwindServer(t, prov, newHangTool())
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)

	body := postLead(t, ts)
	root := ""
	waitFor(t, "the top-level run to start", func() bool {
		for _, e := range srv.cancelReg.ListAll() {
			root = e.RunID
		}
		return root != ""
	})
	dir := seedTreeVolume(t, st, volRoot, root)
	close(prov.proceed)

	waitTopLevelEnded(t, srv, body, root)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the run's end did not purge its tree: stat %v", err)
	}
	if rows, _ := st.EphemeralVolumeListByRun(context.Background(), root); len(rows) != 0 {
		t.Errorf("the run's end left ephemeral rows %+v", rows)
	}
	if n := treeHolds(srv, root); n != 0 {
		t.Errorf("holds on the tree = %d, want none", n)
	}
}

// A poll-mode walk is its starter's background child: it holds the starter's
// tree past the starter's end until the walk's run has closed.
func TestRunTree_PollWalkKeepsItsStartersTreeUntilTheWalkEnds(t *testing.T) {
	h, prov, root := newVolumeDetachHarness(t)
	s := h.startAgent(nil, false)
	dir := withEphemeralWork(t, h, root, &s)
	s.ctx = tools.WithBackground(s.ctx, tools.NewBackground(s.ctx))
	res, err := h.srv.TeamDef(s.ctx, json.RawMessage(`{"op":"run","name":"solo","mode":"poll"}`))
	if err != nil || res.IsError {
		t.Fatalf("TeamDef run poll: %v %s", err, res.Text)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil || out.RunID == "" {
		t.Fatalf("no run_id in %s (%v)", res.Text, err)
	}
	h.memberUp(out.RunID)

	h.endTopLevel(s)
	assertVolumeKept(t, h.st, s.runID, dir, "after the top-level run ended with its poll-mode walk running")

	h.release()
	if walk := h.waitEnded(out.RunID); walk.Status != store.RunCompleted {
		t.Fatalf("walk = %s (%s), want completed", walk.Status, walk.ErrorMsg)
	}
	if writes := prov.results(); len(writes) != 1 || writes[0].IsError {
		t.Fatalf("the member's write to the starter's volume = %+v, want one that succeeded", writes)
	}
	waitVolumePurged(t, h.st, s.runID, dir)
}

// inFlightProvider holds every call until gate is closed, whatever its ctx says —
// a call in flight — and says when one has begun.
type inFlightProvider struct {
	started chan struct{}
	gate    chan struct{}
}

func (g *inFlightProvider) ID() string                  { return "stub" }
func (g *inFlightProvider) Probe(context.Context) error { return nil }
func (g *inFlightProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (g *inFlightProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (g *inFlightProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	select {
	case g.started <- struct{}{}:
	default:
	}
	<-g.gate
	events := answer("resumed")
	ch := make(chan providers.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// A resumed child runs detached from its parent: the root's end leaves the
// tree to it until its goroutine has returned.
func TestRunTree_ResumedChildKeepsItsTreeUntilItHasUnwound(t *testing.T) {
	volRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prov := &inFlightProvider{started: make(chan struct{}, 1), gate: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(prov.gate) }) }
	srv, _ := makeServer(t, prov, resumeTreeConfig("stub", "", volRoot))
	t.Cleanup(func() {
		release()
		srv.cancelReg.Cancel("a_child", "test cleanup")
		waitFor(t, "the resumed child to leave the cancel registry", func() bool {
			_, live := srv.cancelReg.Get("a_child")
			return !live
		})
	})
	parent := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_parent"})
	child := createTreeRun(t, srv, store.RunIdentity{AgentID: "a_child", ParentAgentID: "a_parent", ParentRunID: parent.ID})
	dir := seedTreeVolume(t, srv.store, volRoot, parent.ID)
	pauseMidTurn(t, srv, child)

	resumeOne(t, srv)
	select {
	case <-prov.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the resumed child never started its turn")
	}
	meta := runStateMeta{RunID: parent.ID, IsTopLevel: true, RootRunID: parent.ID}
	srv.finishRunWithCancel(context.Background(), context.Background(), parent.ID, loop.RunResult{}, nil, meta)
	assertVolumeKept(t, srv.store, parent.ID, dir, "after the root ended with a resumed child in its turn")

	release()
	waitRunStatus(t, srv.store, child.ID, store.RunCompleted)
	waitVolumePurged(t, srv.store, parent.ID, dir)
	waitTreeHeldBelow(t, srv, parent.ID, 1)
}
