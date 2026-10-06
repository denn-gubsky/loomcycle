package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// clockProvider stands in for a provider that bounds a run by time
// (UnboundedIterations, as code-js does), so the loop gives its runs a clock.
// It records the clock state each call sees and ends the turn.
type clockProvider struct {
	mu     sync.Mutex
	states []providers.RunClockState
	clocks []bool
}

func (p *clockProvider) ID() string                    { return "clocked" }
func (p *clockProvider) Probe(_ context.Context) error { return nil }
func (p *clockProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *clockProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, UnboundedIterations: true}
}
func (p *clockProvider) Call(ctx context.Context, _ providers.Request) (<-chan providers.Event, error) {
	clock := providers.RunClockFromContext(ctx)
	p.mu.Lock()
	p.states = append(p.states, clock.State())
	p.clocks = append(p.clocks, clock != nil)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	for _, ev := range endTurn() {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (p *clockProvider) calls() ([]providers.RunClockState, []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providers.RunClockState(nil), p.states...), append([]bool(nil), p.clocks...)
}

// TestResumedRun_ContinuesItsRunClock crosses every seam the budget travels
// on a pause: the clock on the parking run's ctx, the pause gate's write into
// the run's record, the record on the row, resume's read of it, the loop's
// clock for the resumed run, and the provider that spends from it. A resumed
// orchestrator that had used 7s of its budget and waited 30s carries on from
// there — not from zero, which would hand it a fresh budget on every restore.
func TestResumedRun_ContinuesItsRunClock(t *testing.T) {
	assertResumeCarriesRunClock(t, func(gate *pauseGate, ctx context.Context) { _ = gate.Park(ctx) })
}

// The same for a run the pause found WAITING — on its children, an operator's
// message, a review verdict — which records itself paused where it waits
// instead of at an iteration boundary. That is the usual place a pause finds
// an orchestrator, so its record must carry the clock too.
func TestResumedRun_ContinuesTheRunClockOfAWaitingRun(t *testing.T) {
	assertResumeCarriesRunClock(t, parkIdle)
}

// parkIdle records a waiting run as paused the way the loop's parks do, and
// holds it until the runtime resumes.
func parkIdle(gate *pauseGate, ctx context.Context) {
	resumed, release, ok := gate.PauseIdle(ctx)
	if !ok {
		return
	}
	<-resumed
	release()
}

// assertResumeCarriesRunClock parks a clocked run with park while the runtime
// is paused, restores it from its row, and asserts the resumed run's clock
// continues from what it had recorded.
func assertResumeCarriesRunClock(t *testing.T, park func(gate *pauseGate, ctx context.Context)) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "clocked", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"orch": {Provider: "clocked", Model: "stub-model", SystemPrompt: "orchestrate", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""

	prov := &clockProvider{}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "runclock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"orch","segments":[{"role":"user","content":[{"type":"trusted-text","text":"start"}]}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	if _, clocks := prov.calls(); len(clocks) != 1 || !clocks[0] {
		t.Fatalf("fixture drifted: the original run had no clock (%v)", clocks)
	}
	run := onlyRun(t, st, extractSessionID(string(body)))

	// Park the run the way a pause does, with a clock that has used 7s and
	// waited 30s.
	mgr := pause.NewManager(st, time.Second)
	mgr.RegisterRun(run.ID)
	defer mgr.DeregisterRun(run.ID)
	gate := &pauseGate{mgr: mgr, store: st, runID: run.ID, saveClock: srv.recordRunClock}
	clock := providers.NewRunClock(time.Now().Add(-7*time.Second), providers.RunClockState{Waited: 30 * time.Second, Wall: 40 * time.Second})
	parkCtx := providers.WithRunClock(context.Background(), clock)
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		for mgr.State() == pause.StateRunning {
			time.Sleep(2 * time.Millisecond)
		}
		park(gate, parkCtx)
	}()
	if _, err := mgr.Pause(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	<-parked

	// Restore it elsewhere: resume from the row.
	parkForResume(t, srv, run.ID)
	if n, warns := srv.ResumePausedRuns(context.Background()); n == 0 {
		t.Fatalf("nothing resumed (warnings: %v)", warns)
	}
	deadline := time.Now().Add(5 * time.Second)
	var states []providers.RunClockState
	for time.Now().Before(deadline) {
		if states, _ = prov.calls(); len(states) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(states) < 2 {
		t.Fatal("the resumed run never called its provider")
	}
	resumed := states[1]
	if resumed.Active < 7*time.Second || resumed.Active > 8*time.Second {
		t.Errorf("the resumed run's clock starts at %s active, want the 7s it had used — "+
			"a resumed run must neither regain nor lose its budget", resumed.Active)
	}
	if resumed.Waited < 30*time.Second {
		t.Errorf("the resumed run's clock reports %s waited, want the 30s it had waited", resumed.Waited)
	}
	// Its lifetime continues too — 40s carried plus the 7s it lived — and the
	// pause it was parked in is not part of it.
	if resumed.Wall < 47*time.Second || resumed.Wall > 48*time.Second {
		t.Errorf("the resumed run's lifetime starts at %s, want the 47s it had lived — "+
			"a resume must not reset the wall limit", resumed.Wall)
	}
}

// Parked by a runtime pause is waiting too: a run whose budget is active time
// does not spend it while the runtime is paused, nor its lifetime.
func TestPauseGatePark_ParksTheRunClock(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := pause.NewManager(st, time.Second)
	const runID = "r_parked"
	mgr.RegisterRun(runID)
	defer mgr.DeregisterRun(runID)
	gate := &pauseGate{mgr: mgr, store: st, runID: runID}
	clock := providers.NewRunClock(time.Now(), providers.RunClockState{})
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		for mgr.State() == pause.StateRunning {
			time.Sleep(2 * time.Millisecond)
		}
		_ = gate.Park(providers.WithRunClock(context.Background(), clock))
	}()
	if _, err := mgr.Pause(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	const pausedFor = 400 * time.Millisecond
	time.Sleep(pausedFor)
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	<-parked
	cs := clock.State()
	if cs.Waited < pausedFor*3/4 {
		t.Fatalf("the run's clock counted %s of a %s pause as waited — the pause was spent as active time", cs.Waited, pausedFor)
	}
	// The operator paused the runtime; the run did not linger. The pause does
	// not count against its lifetime limit.
	if cs.Wall > pausedFor/2 {
		t.Errorf("the run's lifetime grew %s across a %s runtime pause", cs.Wall, pausedFor)
	}
}

// A run the pause finds waiting is parked by the operator just as one at an
// iteration boundary is: the pause does not count against its lifetime, and
// what it has spent is recorded for a snapshot taken at the barrier.
func TestPauseGatePauseIdle_ParksTheRunClock(t *testing.T) {
	st, err := storesqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr := pause.NewManager(st, time.Second)
	const runID = "r_waiting"
	mgr.RegisterRun(runID)
	defer mgr.DeregisterRun(runID)
	var saved atomic.Int32
	gate := &pauseGate{mgr: mgr, store: st, runID: runID,
		saveClock: func(context.Context, string, providers.RunClockState) error { saved.Add(1); return nil }}
	clock := providers.NewRunClock(time.Now(), providers.RunClockState{})
	ctx := providers.WithRunClock(context.Background(), clock)
	// The waiting park's own bracket (parkForChildren is a wait): the budget
	// stops either way — only the pause also stops the lifetime.
	defer providers.BeginWait(ctx)()
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		for mgr.State() == pause.StateRunning {
			time.Sleep(2 * time.Millisecond)
		}
		parkIdle(gate, ctx)
	}()
	if _, err := mgr.Pause(context.Background(), 2*time.Second); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	const pausedFor = 400 * time.Millisecond
	time.Sleep(pausedFor)
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting run never left its pause record")
	}
	if saved.Load() != 1 {
		t.Errorf("the clock was recorded %d times, want once — a snapshot taken at the barrier would carry no clock", saved.Load())
	}
	if cs := clock.State(); cs.Wall > pausedFor/2 {
		t.Errorf("the waiting run's lifetime grew %s across a %s runtime pause", cs.Wall, pausedFor)
	}
}
