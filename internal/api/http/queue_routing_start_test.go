package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// routeProvider answers every call at once and records the models it was
// called with, so a test sees where a run actually ran.
type routeProvider struct {
	id     string
	vision bool

	mu     sync.Mutex
	models []string
}

func (p *routeProvider) ID() string                                   { return p.id }
func (p *routeProvider) Probe(context.Context) error                  { return nil }
func (p *routeProvider) ListModels(context.Context) ([]string, error) { return nil, nil }
func (p *routeProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, SupportsVision: p.vision}
}
func (p *routeProvider) Call(_ context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.models = append(p.models, req.Model)
	p.mu.Unlock()
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "ok"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
	close(ch)
	return ch, nil
}

func (p *routeProvider) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.models...)
}

// forget drops the calls recorded so far, so a later assertion sees only the
// turn under test.
func (p *routeProvider) forget() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.models = nil
}

// queueRouteFixture is a server whose queue-aware agent "worker" routes over
// tier "low": first "busy"/m1, then "other"/m2. busy's chat lane has one slot
// and its calls are measured at ~40 s, so taking that slot (fillBusy) puts its
// estimated wait past the agent's 20 s switch_after_ms. The providers in the
// registry are not lane-wrapped: a run resolved to busy calls it at once, so
// the estimate decides where a run goes and nothing else.
type queueRouteFixture struct {
	s     *Server
	sched *concurrency.CallScheduler
	busy  *routeProvider
	other *routeProvider
}

func newQueueRouteFixture(t *testing.T, otherHasVision bool) *queueRouteFixture {
	t.Helper()
	cands := []config.TierCandidate{{Provider: "busy", Model: "m1"}, {Provider: "other", Model: "m2"}}
	cfg := &config.Config{
		ProviderPriority: []string{"busy", "other"},
		Tiers:            map[string][]config.TierCandidate{"low": cands},
		Agents: map[string]config.AgentDef{"worker": {
			Tier: "low", SystemPrompt: "be brief", Tools: []string{},
			Routing: &config.AgentRouting{QueueAware: true, SwitchAfterMS: 20000},
		}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	cfg.Env.AuthToken = "" // open mode
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "route.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &queueRouteFixture{busy: &routeProvider{id: "busy", vision: true}, other: &routeProvider{id: "other", vision: otherHasVision}}
	f.s = New(cfg, &multiResolver{byID: map[string]providers.Provider{"busy": f.busy, "other": f.other}},
		[]tools.Tool{}, concurrency.New(4, 4, 5*time.Second), st)
	r := resolve.NewResolver([]string{"busy", "other"}, map[string][]resolve.Candidate{
		"low": {{Provider: "busy", Model: "m1"}, {Provider: "other", Model: "m2"}},
	})
	r.SetReachable("busy", true, []string{"m1"}, "")
	r.SetReachable("other", true, []string{"m2"}, "")
	f.s.SetResolver(r)
	f.sched = concurrency.NewCallScheduler(chatCaps(map[string]int{"busy": 1}))
	f.s.SetCallScheduler(f.sched)
	f.s.throughput = throughput.New(&config.Config{}, nil, nil)
	for i := 0; i < 5; i++ {
		f.s.throughput.Observe(throughput.Sample{RunID: fmt.Sprintf("seed%d", i), At: time.Now(),
			Key: throughput.Key{Provider: "busy", Model: "m1"}, Output: 100,
			CallTiming: providers.CallTiming{DurationMs: 40000}})
	}
	return f
}

// fillBusy takes busy's one chat slot for another run until the test ends.
func (f *queueRouteFixture) fillBusy(t *testing.T) {
	t.Helper()
	release, err := f.sched.Acquire(context.Background(), "busy", config.ModelKindChat, concurrency.CallInfo{Owner: "someone-else", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

func imageUserSeg() []loop.PromptSegment {
	return []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{
		{Type: "trusted-text", Text: "what is in this picture?"},
		{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgo="},
	}}}
}

const imageSegmentsJSON = `[{"role":"user","content":[{"type":"trusted-text","text":"what is in this picture?"},{"type":"image","media_type":"image/png","data":"iVBORw0KGgo="}]}]`

// postRunStream POSTs body to path and returns the whole SSE body. It does not
// fail the test itself, so a goroutine can call it.
func postRunStream(ts *httptest.Server, path, body string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, out)
	}
	return string(out), nil
}

func postRunBody(t *testing.T, ts *httptest.Server, path, body string) string {
	t.Helper()
	out, err := postRunStream(ts, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// awaitPost waits for a postRunStream started on a goroutine.
func awaitPost(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never finished")
	}
}

// assertRanOnBusy checks a run with an image was not passed over to "other",
// which cannot take images, but stayed on the busy vision-capable provider and
// ran there.
func (f *queueRouteFixture) assertRanOnBusy(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("run failed: %v (passed over to a provider that cannot take its image)", err)
	}
	if got := f.other.calls(); len(got) != 0 {
		t.Errorf("the text-only provider was called: %v", got)
	}
	if got := f.busy.calls(); len(got) == 0 {
		t.Error("the busy vision-capable provider was never called: the run did not run where it could")
	}
}

// runOnceToEnd runs in to its end and returns RunOnce's error, or the first
// error the run reported on its stream: a loop that refuses the run reports
// it there.
func runOnceToEnd(s *Server, in runner.RunInput) error {
	var (
		mu     sync.Mutex
		failed error
	)
	err := s.RunOnce(context.Background(), in, runner.RunCallbacks{OnEvent: func(ev providers.Event) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Type == providers.EventError && failed == nil {
			failed = fmt.Errorf("stream: %s", ev.Error)
		}
	}})
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	return failed
}

// finishFirstTurn runs one turn of worker with segs on an idle busy and
// returns its session; the calls it made are forgotten.
func (f *queueRouteFixture) finishFirstTurn(t *testing.T, segs []loop.PromptSegment) string {
	t.Helper()
	first := startRunOnce(f.s, context.Background(), runner.RunInput{Agent: "worker", Segments: segs})
	_, firstRun := first.ids(t)
	if err := first.wait(t); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	run := waitRunStatus(t, f.s.store, firstRun, store.RunCompleted)
	if got := f.busy.calls(); len(got) != 1 {
		t.Fatalf("first turn's calls on busy = %v, want one (busy is first and idle)", got)
	}
	f.busy.forget()
	return run.SessionID
}

// A queue-aware run carrying an image is never passed over from a busy
// provider that can take it to one that cannot: it would only have waited,
// and on the text-only provider the loop's vision gate fails it instead.
func TestRunOnce_QueueAwareImageRunIsNotPassedToATextOnlyProvider(t *testing.T) {
	f := newQueueRouteFixture(t, false)
	f.fillBusy(t)
	f.assertRanOnBusy(t, runOnceToEnd(f.s, runner.RunInput{Agent: "worker", Segments: imageUserSeg()}))
}

func TestHandleRuns_QueueAwareImageRunIsNotPassedToATextOnlyProvider(t *testing.T) {
	f := newQueueRouteFixture(t, false)
	f.fillBusy(t)
	ts := httptest.NewServer(f.s.Mux())
	defer ts.Close()
	body := postRunBody(t, ts, "/v1/runs", `{"agent":"worker","segments":`+imageSegmentsJSON+`}`)
	var err error
	if strings.Contains(body, "does not support image input") {
		err = fmt.Errorf("stream: %s", body)
	}
	f.assertRanOnBusy(t, err)
}

// An image earlier in the session counts as one in the new input: the loop
// replays it into every later turn, so a continuation needs vision too.
func TestRunOnce_QueueAwareContinuationOfAnImageSessionIsNotPassedToATextOnlyProvider(t *testing.T) {
	f := newQueueRouteFixture(t, false)
	sessionID := f.finishFirstTurn(t, imageUserSeg())
	f.fillBusy(t)
	f.assertRanOnBusy(t, runOnceToEnd(f.s, runner.RunInput{Agent: "worker", SessionID: sessionID, Segments: oneUserSeg("and now?")}))
}

func TestHandleMessages_QueueAwareContinuationOfAnImageSessionIsNotPassedToATextOnlyProvider(t *testing.T) {
	f := newQueueRouteFixture(t, false)
	sessionID := f.finishFirstTurn(t, imageUserSeg())
	f.fillBusy(t)
	ts := httptest.NewServer(f.s.Mux())
	defer ts.Close()
	body := postRunBody(t, ts, "/v1/sessions/"+sessionID+"/messages",
		`{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"and now?"}]}]}`)
	var err error
	if strings.Contains(body, "does not support image input") {
		err = fmt.Errorf("stream: %s", body)
	}
	f.assertRanOnBusy(t, err)
}

// queueBehindHeldSlot gives the fixture's server a global run cap of one and
// takes that slot, so the next run queues; the returned func frees it.
func (f *queueRouteFixture) queueBehindHeldSlot(t *testing.T) func() {
	t.Helper()
	f.s.sem = concurrency.New(1, 8, 0)
	release, err := f.s.sem.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	free := func() { once.Do(release) }
	t.Cleanup(free)
	return free
}

// waitQueuedAgent waits for the run with agentID to be queued.
func waitQueuedAgent(t *testing.T, st store.Store, agentID string) store.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		run, err := st.GetRunByAgentID(context.Background(), agentID)
		if err == nil && run.Status == store.RunQueued {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never queued (status %q, err %v)", agentID, run.Status, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// assertResolvedAfterTheWait checks a queue-aware run that queued while busy
// was free, and started after busy filled up, ran on "other": its provider was
// chosen from the lanes as they were when it started, not when it arrived.
func (f *queueRouteFixture) assertResolvedAfterTheWait(t *testing.T, runID string) {
	t.Helper()
	waitRunStatus(t, f.s.store, runID, store.RunCompleted)
	if got := f.other.calls(); len(got) != 1 || got[0] != "m2" {
		t.Errorf("other's calls = %v, want one call with m2", got)
	}
	if got := f.busy.calls(); len(got) != 0 {
		t.Errorf("busy was called %v: the run kept the provider chosen when it arrived", got)
	}
}

// A queue-aware run that waited for a run slot is resolved again when it
// starts: its provider was chosen from the lane estimates when it arrived,
// which say nothing about the lanes minutes later, and the run makes no move
// of its own before its second call.
func TestRunOnce_QueueAwareRunThatQueuedIsResolvedAgainWhenItStarts(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	free := f.queueBehindHeldSlot(t)

	started := startRunOnce(f.s, context.Background(), runner.RunInput{Agent: "worker", AgentID: "a_requeue", Segments: oneUserSeg("go")})
	_, runID := started.ids(t)
	waitQueuedAgent(t, f.s.store, "a_requeue")
	f.fillBusy(t)
	free()
	if err := started.wait(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	f.assertResolvedAfterTheWait(t, runID)
}

func TestHandleRuns_QueueAwareRunThatQueuedIsResolvedAgainWhenItStarts(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	free := f.queueBehindHeldSlot(t)
	ts := httptest.NewServer(f.s.Mux())
	defer ts.Close()

	done := make(chan error, 1)
	go func() {
		_, err := postRunStream(ts, "/v1/runs", `{"agent":"worker","agent_id":"a_requeue_http","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`)
		done <- err
	}()
	run := waitQueuedAgent(t, f.s.store, "a_requeue_http")
	f.fillBusy(t)
	free()
	awaitPost(t, done)
	f.assertResolvedAfterTheWait(t, run.ID)
}

func TestHandleMessages_QueueAwareRunThatQueuedIsResolvedAgainWhenItStarts(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	sessionID := f.finishFirstTurn(t, oneUserSeg("first turn"))
	free := f.queueBehindHeldSlot(t)
	ts := httptest.NewServer(f.s.Mux())
	defer ts.Close()
	done := make(chan error, 1)
	go func() {
		_, err := postRunStream(ts, "/v1/sessions/"+sessionID+"/messages",
			`{"agent_id":"a_requeue_msg","segments":[{"role":"user","content":[{"type":"trusted-text","text":"again"}]}]}`)
		done <- err
	}()
	run := waitQueuedAgent(t, f.s.store, "a_requeue_msg")
	f.fillBusy(t)
	free()
	awaitPost(t, done)
	f.assertResolvedAfterTheWait(t, run.ID)
}
