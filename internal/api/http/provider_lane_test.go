package http

import (
	"context"
	"encoding/json"
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
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A provider's max_concurrent counts model CALLS. These tests drive whole runs
// through the server and watch the provider's chat lane from outside: the cap
// holds across a run tree, a run holds nothing while it is not calling the
// model, and no kind of run escapes the count.
//
// The server never acquires a lane itself. The registry hands out every
// provider wrapped in its lane (cmd/loomcycle); lanedServer is that registry
// for a test.

// multiResolver maps a resolved provider id to its own provider instance.
type multiResolver struct{ byID map[string]providers.Provider }

func (r *multiResolver) Get(id string) (providers.Provider, error) {
	if p, ok := r.byID[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no provider %q", id)
}

// chatCaps is the scheduler config that caps each named provider's chat calls.
func chatCaps(caps map[string]int) map[string]concurrency.ProviderLanesConfig {
	out := make(map[string]concurrency.ProviderLanesConfig, len(caps))
	for id, n := range caps {
		out[id] = concurrency.ProviderLanesConfig{Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: {MaxConcurrent: n}}}
	}
	return out
}

// lanedServer builds a server whose providers are gated the way the binary's
// are: each one wrapped in its call lane under its provider id, all on one
// scheduler, which the server also reports and reloads. The global run cap is
// high enough never to be the limit.
func lanedServer(t *testing.T, cfg *config.Config, caps map[string]int, provs map[string]providers.Provider, extra ...tools.Tool) (*Server, *concurrency.CallScheduler) {
	t.Helper()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "lane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sched := concurrency.NewCallScheduler(chatCaps(caps))
	wrapped := make(map[string]providers.Provider, len(provs))
	for id, p := range provs {
		wrapped[id] = lanegate.Wrap(id, p, sched)
	}
	srv := New(cfg, &multiResolver{byID: wrapped}, extra, concurrency.New(16, 16, 5*time.Second), st)
	srv.SetCallScheduler(sched)
	return srv, sched
}

func chatLaneOf(sched *concurrency.CallScheduler, id string) concurrency.LaneStats {
	return sched.Stats()[id].Lanes[config.ModelKindChat]
}

// waitForLane polls a provider's chat lane until (active, queued) match —
// poll-until instead of a fixed sleep so -race doesn't flake.
func waitForLane(t *testing.T, sched *concurrency.CallScheduler, id string, wantActive, wantQueued int) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if st := chatLaneOf(sched, id); st.Active == wantActive && st.Queued == wantQueued {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := chatLaneOf(sched, id)
	t.Fatalf("lane %s: want active=%d queued=%d, got active=%d queued=%d", id, wantActive, wantQueued, st.Active, st.Queued)
}

func assertLaneIdle(t *testing.T, sched *concurrency.CallScheduler, id string) {
	t.Helper()
	if st := chatLaneOf(sched, id); st.Active != 0 || st.Queued != 0 || st.Reserved != 0 {
		t.Errorf("final lane %s: active=%d queued=%d reserved=%d, want 0/0/0", id, st.Active, st.Queued, st.Reserved)
	}
}

func laneRunBody(agent, agentID string) string {
	return `{"agent":"` + agent + `","agent_id":"` + agentID +
		`","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
}

// postLaneRun runs one agent to completion and returns the SSE body. The client
// timeout turns a run that never finishes into a failure, not a hung test.
func postLaneRun(t *testing.T, ts *httptest.Server, body string) string {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("post: %v", err)
		return ""
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("read: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d body=%s", resp.StatusCode, out)
	}
	return string(out)
}

func textThenDone(text string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventText, Text: text},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

func toolThenDone(id, name, input string) []providers.Event {
	return []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
}

// flightMeter counts the model calls in flight on a host, however many
// providers and models share it, and remembers the most it saw at once.
type flightMeter struct {
	// holdUntil (0 = off) makes the calls that ask for it RENDEZVOUS: each
	// blocks until that many are in flight together (or a generous deadline)
	// before it does its work, so the observed peak reaches the cap whatever
	// the scheduler's jitter. A cap that admits too few leaves the deadline to
	// fire (peak < holdUntil); one that admits too many shows peak > holdUntil.
	holdUntil int

	mu       sync.Mutex
	inFlight int
	peak     int
	arrived  int
	barrier  chan struct{}
}

func (m *flightMeter) enter(ctx context.Context, rendezvous bool) {
	m.mu.Lock()
	m.inFlight++
	if m.inFlight > m.peak {
		m.peak = m.inFlight
	}
	var barrier chan struct{}
	if rendezvous && m.holdUntil > 0 {
		if m.barrier == nil {
			m.barrier = make(chan struct{})
		}
		barrier = m.barrier
		m.arrived++
		if m.arrived == m.holdUntil { // fires exactly once: the batch goes together
			close(m.barrier)
		}
	}
	m.mu.Unlock()
	if barrier != nil {
		select {
		case <-barrier:
		case <-time.After(2 * time.Second): // an under-admitting cap fails the test instead of hanging it
		case <-ctx.Done():
		}
	}
}

func (m *flightMeter) leave() {
	m.mu.Lock()
	m.inFlight--
	m.mu.Unlock()
}

func (m *flightMeter) peakInFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// meteredProvider is one host serving several models. Each model has a script
// (one event list per call, then its fallback); every call is counted on the
// meter from Call until just before its last events are sent, so a call the
// lane has already let go of is never still counted.
type meteredProvider struct {
	meter *flightMeter
	delay time.Duration
	// rendezvous names the models whose calls wait for each other (see
	// flightMeter.holdUntil).
	rendezvous map[string]bool

	mu       sync.Mutex
	scripts  map[string][][]providers.Event
	fallback map[string][]providers.Event
	calls    map[string]int
}

func (p *meteredProvider) ID() string                    { return "metered" }
func (p *meteredProvider) Probe(_ context.Context) error { return nil }
func (p *meteredProvider) ListModels(_ context.Context) ([]string, error) {
	return []string{"m"}, nil
}
func (p *meteredProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}

func (p *meteredProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	idx := p.calls[req.Model]
	p.calls[req.Model]++
	events := p.fallback[req.Model]
	if idx < len(p.scripts[req.Model]) {
		events = p.scripts[req.Model][idx]
	}
	p.mu.Unlock()
	if events == nil {
		events = textThenDone("ok")
	}
	p.meter.enter(ctx, p.rendezvous[req.Model])
	ch := make(chan providers.Event, len(events))
	go func() {
		defer close(ch)
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
		}
		p.meter.leave()
		for _, ev := range events {
			ch <- ev
		}
	}()
	return ch, nil
}

func (p *meteredProvider) callsTo(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[model]
}

func fanoutInput(agent string, n int) string {
	spawns := make([]string, n)
	for i := range spawns {
		spawns[i] = fmt.Sprintf(`{"name":%q,"prompt":"task-%d"}`, agent, i)
	}
	return `{"op":"parallel_spawn","spawns":[` + strings.Join(spawns, ",") + `]}`
}

// TestProviderLane_FanoutParentAndSameProviderChildrenStayWithinTheCap — a
// parent on provider P (max_concurrent 2) fans out five children that also run
// on P. At most two model calls are ever in flight on P, the parent's included,
// and every child completes.
//
// This is the hole the run-level gate had: a parent held its slot while it
// awaited its children, so children on the same provider had to run UNGATED or
// deadlock behind it — and a fan-out put five calls on a provider capped at
// two. Fail-before: with the lane not applied to those calls, peak is 5.
func TestProviderLane_FanoutParentAndSameProviderChildrenStayWithinTheCap(t *testing.T) {
	const limit, workers = 2, 5
	host := &meteredProvider{
		meter:      &flightMeter{holdUntil: limit},
		delay:      40 * time.Millisecond,
		rendezvous: map[string]bool{"worker-m": true},
		scripts: map[string][][]providers.Event{
			"parent-m": {toolThenDone("tu_fanout", "Agent", fanoutInput("worker", workers)), textThenDone("fan-out done")},
		},
		fallback: map[string][]providers.Event{"worker-m": textThenDone("worked")},
	}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		// max_concurrent_children above the cap, so the Agent tool's own
		// fan-out limit is not what bounds the calls — the lane must be.
		"parent": {Provider: "P", Model: "parent-m", Tools: []string{"Agent"}, MaxConcurrentChildren: workers + 5},
		"worker": {Provider: "P", Model: "worker-m", Tools: []string{}},
	}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": limit}, map[string]providers.Provider{"P": host})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	body := postLaneRun(t, ts, laneRunBody("parent", "fan-parent"))
	if !strings.Contains(body, "fan-out done") {
		t.Fatalf("the parent did not finish its final turn; body=\n%s", body)
	}
	if peak := host.meter.peakInFlight(); peak != limit {
		t.Errorf("peak model calls in flight on P = %d, want exactly max_concurrent=%d (a parent and its same-provider children share the cap)", peak, limit)
	}
	if n := host.callsTo("worker-m"); n != workers {
		t.Errorf("worker calls = %d, want %d (every child ran)", n, workers)
	}
	children, err := srv.store.ListRunsByParentAgentID(context.Background(), "fan-parent")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(children) != workers {
		t.Errorf("child runs = %d, want %d", len(children), workers)
	}
	for _, c := range children {
		if c.Status != store.RunCompleted {
			t.Errorf("child %s ended %q, want completed", c.ID, c.Status)
		}
	}
	assertLaneIdle(t, sched, "P")
}

// TestProviderLane_RunHoldsNoSlotWhileAToolExecutes — on a provider capped at
// one call, a run that is inside a tool call holds nothing: a second run makes
// its call and finishes while the first is still in its tool. The two runs
// interleave their calls; neither waits for the other's whole conversation.
//
// Fail-before: a slot that outlives the call keeps the lane at active=1 while
// the tool runs, and the second run cannot start its call until the first run
// has ended.
func TestProviderLane_RunHoldsNoSlotWhileAToolExecutes(t *testing.T) {
	hang := newHangTool()
	host := &meteredProvider{
		meter: &flightMeter{},
		scripts: map[string][][]providers.Event{
			"tooler-m": {toolThenDone("tu_hang", "Hang", `{}`), textThenDone("tooler done")},
		},
		fallback: map[string][]providers.Event{"quick-m": textThenDone("quick done")},
	}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"tooler": {Provider: "P", Model: "tooler-m", Tools: []string{"Hang"}},
		"quick":  {Provider: "P", Model: "quick-m", Tools: []string{}},
	}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": host}, hang)
	ts := httptest.NewServer(srv.Mux())
	var releaseOnce sync.Once
	releaseTool := func() { releaseOnce.Do(func() { close(hang.gate) }) }
	t.Cleanup(ts.Close)
	t.Cleanup(releaseTool) // LIFO: the tool is released before the server closes

	toolerBody := make(chan string, 1)
	go func() { toolerBody <- postLaneRun(t, ts, laneRunBody("tooler", "tooler-1")) }()
	hang.waitEntered(t)

	// The first run is mid-run, inside its tool. It is not calling the model.
	if st := chatLaneOf(sched, "P"); st.Active != 0 {
		t.Fatalf("lane active = %d while the run executes a tool, want 0 (a run holds a slot only during a model call)", st.Active)
	}
	// So another run on the same cap-1 provider runs start to finish now.
	if body := postLaneRun(t, ts, laneRunBody("quick", "quick-1")); !strings.Contains(body, "quick done") {
		t.Fatalf("the second run did not complete while the first was in its tool; body=\n%s", body)
	}
	select {
	case <-toolerBody:
		t.Fatal("the first run finished before its tool was released")
	default:
	}

	releaseTool()
	if body := <-toolerBody; !strings.Contains(body, "tooler done") {
		t.Fatalf("the first run did not finish after its tool returned; body=\n%s", body)
	}
	if peak := host.meter.peakInFlight(); peak != 1 {
		t.Errorf("peak model calls in flight = %d, want 1", peak)
	}
	assertLaneIdle(t, sched, "P")
}

// TestProviderLane_InteractiveRunCallsAreCounted — an interactive run executes
// detached from the request that started it. Its model calls take a slot like
// any other, and a second run's call queues behind one.
//
// Fail-before: a detached run gave its run-level slot back at hand-off, so its
// calls were never counted — the lane stays at active=0 while it calls.
func TestProviderLane_InteractiveRunCallsAreCounted(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"term":  {Provider: "P", Model: "m", UnboundedIterations: true},
		"quick": {Provider: "P", Model: "m", Tools: []string{}},
	}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": prov})
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(prov.release) }) }
	var wg sync.WaitGroup
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		releaseModel()
		// End the interactive run: parked at awaiting_input it would otherwise
		// keep its stream, and so the server, open.
		if resp, err := http.Post(ts.URL+"/v1/agents/term-1/cancel", "application/json", nil); err == nil {
			resp.Body.Close()
		}
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"term","agent_id":"term-1","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`))
		if err != nil {
			t.Errorf("post interactive: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	// The detached run's call is in flight and counted.
	waitForLane(t, sched, "P", 1, 0)

	// A second run's call waits for it.
	quick := make(chan string, 1)
	go func() { quick <- postLaneRun(t, ts, laneRunBody("quick", "quick-1")) }()
	waitForLane(t, sched, "P", 1, 1)

	releaseModel()
	if body := <-quick; !strings.Contains(body, "ok") {
		t.Fatalf("the queued run did not complete; body=\n%s", body)
	}
	waitForLane(t, sched, "P", 0, 0)
}

// TestProviderLane_BoundedChildQueueWaitNotCountedAgainstTimeout — a parent
// spawns one child with timeout_ms 300 on a provider whose only slot is taken
// for about 900 ms. The child's call waits for the lane, the wait does not
// count against its timeout_ms, and it completes once the slot frees. This
// also proves the clock the Agent tool sets on the child's ctx reaches the
// lane wait. Fail-before: without the wait reported to that clock, the child
// times out at 300 ms.
func TestProviderLane_BoundedChildQueueWaitNotCountedAgainstTimeout(t *testing.T) {
	parentProv := &scriptedProvider{scripts: [][]providers.Event{
		toolThenDone("tu_spawn", "Agent", `{"name":"child","prompt":"go","timeout_ms":300}`),
		textThenDone("parent done"),
	}}
	childProv := &scriptedProvider{defaultS: textThenDone("child answered")}
	cfg := makeBaseConfig()
	cfg.Agents = map[string]config.AgentDef{
		"parent": {Provider: "parent-prov", Model: "m", Tools: []string{"Agent"}, SystemPrompt: "parent"},
		"child":  {Provider: "Q", Model: "m", Tools: []string{}, SystemPrompt: "child"},
	}
	srv, sched := lanedServer(t, cfg, map[string]int{"Q": 1},
		map[string]providers.Provider{"parent-prov": parentProv, "Q": childProv})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	// Another call holds Q's only slot for three times the child's timeout_ms.
	other, err := sched.Acquire(context.Background(), "Q", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take Q: %v", err)
	}
	var once sync.Once
	releaseOther := func() { once.Do(other) }
	defer releaseOther()
	time.AfterFunc(900*time.Millisecond, releaseOther)

	body := postLaneRun(t, ts, laneRunBody("parent", "parent-bounded"))
	if strings.Contains(body, "timed out") {
		t.Errorf("the child's lane wait counted against its timeout_ms; body=\n%s", body)
	}
	children, err := srv.store.ListRunsByParentAgentID(context.Background(), "parent-bounded")
	if err != nil {
		t.Fatalf("ListRunsByParentAgentID: %v", err)
	}
	if len(children) != 1 || children[0].Status != store.RunCompleted {
		t.Fatalf("child runs = %+v, want one completed run", children)
	}
	if childProv.calls.Load() != 1 {
		t.Errorf("child model calls = %d, want 1", childProv.calls.Load())
	}
	assertLaneIdle(t, sched, "Q")
}

// TestProviderLane_OverCapRunsWaitAndBatchToTheCap — more runs than the cap
// all go through: none is refused, at most max_concurrent calls are in flight,
// and the peak reaches the cap.
func TestProviderLane_OverCapRunsWaitAndBatchToTheCap(t *testing.T) {
	const limit, total = 2, 5
	host := &meteredProvider{meter: &flightMeter{holdUntil: limit}, delay: 40 * time.Millisecond, rendezvous: map[string]bool{"m": true}}
	cfg := makeBaseConfig()
	cfg.Agents["capped"] = config.AgentDef{Provider: "P", Model: "m", Tools: []string{}}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": limit}, map[string]providers.Provider{"P": host})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postLaneRun(t, ts, laneRunBody("capped", fmt.Sprintf("capped-%d", i)))
		}()
	}
	wg.Wait()
	if peak := host.meter.peakInFlight(); peak != limit {
		t.Errorf("peak model calls in flight = %d, want exactly max_concurrent=%d", peak, limit)
	}
	if n := host.callsTo("m"); n != total {
		t.Errorf("model calls = %d, want %d (no run was refused)", n, total)
	}
	assertLaneIdle(t, sched, "P")
}

// TestProviderLane_UncappedProviderIsNotSerialised — with no max_concurrent
// anywhere the wrapper is inert: calls overlap freely and the server reports no
// provider lanes.
func TestProviderLane_UncappedProviderIsNotSerialised(t *testing.T) {
	const n = 4
	host := &meteredProvider{meter: &flightMeter{holdUntil: n}, delay: 20 * time.Millisecond, rendezvous: map[string]bool{"m": true}}
	cfg := makeBaseConfig()
	cfg.Agents["free"] = config.AgentDef{Provider: "P", Model: "m", Tools: []string{}}
	srv, sched := lanedServer(t, cfg, nil, map[string]providers.Provider{"P": host})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postLaneRun(t, ts, laneRunBody("free", fmt.Sprintf("free-%d", i)))
		}()
	}
	wg.Wait()
	if peak := host.meter.peakInFlight(); peak != n {
		t.Errorf("peak = %d; with no cap all %d calls should overlap", peak, n)
	}
	if sched.Stats() != nil || providerLaneSnapshot(srv) != nil {
		t.Errorf("an uncapped deployment reports lanes: %v", sched.Stats())
	}
}

// TestProviderLane_ReloadedCapAppliesToTheNextCall — max_concurrent is looked
// up per call, so a config reload takes effect on the next call of a provider
// that was built (and wrapped) before it: a cap added by a reload gates it, a
// cap raised by a reload admits a call that was waiting. The stats endpoint
// reports the lane throughout.
func TestProviderLane_ReloadedCapAppliesToTheNextCall(t *testing.T) {
	prov := &pausableProvider{release: make(chan struct{}), finalText: "ok"}
	cfg := makeBaseConfig()
	cfg.Agents["capped"] = config.AgentDef{Provider: "P", Model: "m", Tools: []string{}}
	// Nothing is capped at boot.
	srv, sched := lanedServer(t, cfg, nil, map[string]providers.Provider{"P": prov})
	ts := httptest.NewServer(srv.Mux())
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(prov.release) }) }
	var wg sync.WaitGroup
	t.Cleanup(ts.Close)
	t.Cleanup(func() { releaseModel(); wg.Wait() })

	withCap := func(n int) *config.Config {
		next := *cfg
		next.Providers = map[string]config.ProviderConfig{"P": {MaxConcurrent: n}}
		return &next
	}
	run := func(id string) {
		wg.Add(1)
		go func() { defer wg.Done(); postLaneRun(t, ts, laneRunBody("capped", id)) }()
	}

	// Reload: P is now capped at one call.
	srv.ReloadConcurrency(withCap(1))
	run("a")
	waitForLane(t, sched, "P", 1, 0)
	run("b")
	waitForLane(t, sched, "P", 1, 1)

	resp, err := http.Get(ts.URL + "/v1/_concurrency/stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	var stats concurrencyStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	resp.Body.Close()
	if got := stats.Providers["P"]; got.Active != 1 || got.Queued != 1 {
		t.Errorf("/v1/_concurrency/stats providers.P = %+v, want active=1 queued=1 (the chat lane's calls)", got)
	}

	// Reload: the cap is raised, and the waiting call goes at once.
	srv.ReloadConcurrency(withCap(2))
	waitForLane(t, sched, "P", 2, 0)

	releaseModel()
	wg.Wait()
	waitForLane(t, sched, "P", 0, 0)
}

// pausedResumerRun files a paused run of agent "resumer" (provider
// "scripted") whose transcript ends on a pending tool_result, ready for
// ResumePausedRuns. Mirrors TestResumePausedRuns_ResumesMidConversationToCompletion.
func pausedResumerRun(t *testing.T, srv *Server) store.Run {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_resume_wait", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
	})
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{
		Type:    providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read", Input: json.RawMessage(`{"path":"/x"}`)},
	})
	appendResumeEvent(t, srv, run.ID, "tool_result", providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read"},
		Text:    "FILE CONTENTS",
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	return run
}

// resumerServer builds a server with one agent, "resumer", on provider
// "scripted". sched, when not nil, gates that provider's calls.
func resumerServer(t *testing.T, sched *concurrency.CallScheduler) (*Server, *scriptedProvider) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "resumed and done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}
	srv, _ := makeServer(t, lanegate.Wrap("scripted", prov, sched), cfg)
	srv.SetCallScheduler(sched)
	return srv, prov
}

// assertRunStillPending fails if the run reached a terminal state.
func assertRunStillPending(t *testing.T, srv *Server, runID string) {
	t.Helper()
	got, err := srv.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status == store.RunFailed || got.Status == store.RunCompleted {
		t.Fatalf("run reached %q while its slot was taken (err=%q), want it still waiting", got.Status, got.ErrorMsg)
	}
}

// TestResumePausedRuns_SaturatedProviderWaitsInsteadOfFailing — a resumed run
// whose provider is saturated waits for its model call's slot and completes
// once one frees. It was admitted before its pause; a resume must never fail
// it for a provider that is merely busy.
func TestResumePausedRuns_SaturatedProviderWaitsInsteadOfFailing(t *testing.T) {
	sched := concurrency.NewCallScheduler(chatCaps(map[string]int{"scripted": 1}))
	srv, prov := resumerServer(t, sched)
	other, err := sched.Acquire(context.Background(), "scripted", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the provider slot: %v", err)
	}
	var once sync.Once
	defer once.Do(other)
	run := pausedResumerRun(t, srv)

	if n, warnings := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	waitForLane(t, sched, "scripted", 1, 1)
	time.Sleep(100 * time.Millisecond)
	assertRunStillPending(t, srv, run.ID)
	if n := prov.calls.Load(); n != 0 {
		t.Fatalf("the model was called %d times while the provider's only slot was taken", n)
	}

	once.Do(other)
	waitRunCompleted(t, srv, run.ID)
	if prov.calls.Load() < 1 {
		t.Error("provider never called — the loop wasn't resumed")
	}
	waitForLane(t, sched, "scripted", 0, 0)
}

// TestResumePausedRuns_SaturatedGlobalSlotWaitsInsteadOfFailing — the same
// for the global run cap: every slot taken and the queue full, the resumed run
// waits (no queue timeout, no per-user cap) and completes once a slot frees.
// Before, it was marked failed "resume: acquire run slot".
func TestResumePausedRuns_SaturatedGlobalSlotWaitsInsteadOfFailing(t *testing.T) {
	srv, _ := resumerServer(t, nil)
	srv.sem = concurrency.New(1, 0, 10*time.Millisecond).WithPerUserCap(1)
	other, err := srv.sem.AcquireForUser(context.Background(), "", "alice")
	if err != nil {
		t.Fatalf("take the global slot: %v", err)
	}
	var once sync.Once
	defer once.Do(other)
	run := pausedResumerRun(t, srv)

	if n, warnings := srv.ResumePausedRuns(context.Background()); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	waitForQueued(t, srv.sem, 1)
	time.Sleep(100 * time.Millisecond) // 10x the semaphore's queue timeout
	assertRunStillPending(t, srv, run.ID)

	once.Do(other)
	waitRunCompleted(t, srv, run.ID)
	// The resumed run gives its slot back in a defer that runs after its row
	// is marked completed, so the slot may still be held for a moment here.
	end := time.Now().Add(2 * time.Second)
	for st := srv.sem.Stats(); st.Active != 0 || st.Queued != 0; st = srv.sem.Stats() {
		if time.Now().After(end) {
			t.Fatalf("final global: active=%d queued=%d, want 0/0", st.Active, st.Queued)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForQueued(t *testing.T, sem *concurrency.Semaphore, want int) {
	t.Helper()
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if sem.Stats().Queued == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("global queued = %d, want %d", sem.Stats().Queued, want)
}

// TestProviderLane_AWaitingCallIsReportedOnItsRun — a run whose model call has
// to wait for its provider's lane reports it: call_queued (the provider, the
// lane kind, why, its place) then call_admitted (how long), on its live stream
// and in its transcript. A run whose call gets the slot at once reports
// neither.
func TestProviderLane_AWaitingCallIsReportedOnItsRun(t *testing.T) {
	host := &meteredProvider{meter: &flightMeter{}, delay: 150 * time.Millisecond}
	cfg := makeBaseConfig()
	cfg.Agents["capped"] = config.AgentDef{Provider: "P", Model: "m", Tools: []string{}}
	srv, sched := lanedServer(t, cfg, map[string]int{"P": 1}, map[string]providers.Provider{"P": host})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	bodies := make([]string, 2)
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bodies[i] = postLaneRun(t, ts, laneRunBody("capped", fmt.Sprintf("call-wait-%d", i)))
		}()
	}
	wg.Wait()
	assertLaneIdle(t, sched, "P")

	waited := -1
	for i, b := range bodies {
		if strings.Contains(b, "event: call_queued") {
			if waited >= 0 {
				t.Fatalf("both runs reported a wait on a lane of one slot")
			}
			waited = i
		}
	}
	if waited < 0 {
		t.Fatalf("neither run reported its call's wait:\n%s\n---\n%s", bodies[0], bodies[1])
	}
	b := bodies[waited]
	if !strings.Contains(b, `"admission":{"reason":"lane","position":1,"provider":"P","kind":"chat"}`) {
		t.Errorf("call_queued payload missing or wrong in:\n%s", b)
	}
	if !strings.Contains(b, "event: call_admitted") || strings.Index(b, "event: call_admitted") < strings.Index(b, "event: call_queued") {
		t.Errorf("call_admitted missing or before call_queued in:\n%s", b)
	}
	if strings.Contains(bodies[1-waited], "event: call_") {
		t.Errorf("the run that got the slot at once reported a call wait:\n%s", bodies[1-waited])
	}

	run, err := srv.store.GetRunByAgentID(context.Background(), fmt.Sprintf("call-wait-%d", waited))
	if err != nil {
		t.Fatal(err)
	}
	evs, err := srv.store.GetRunEventsSince(context.Background(), run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range evs {
		if strings.HasPrefix(ev.Type, "call_") {
			types = append(types, ev.Type)
		}
	}
	if strings.Join(types, ",") != "call_queued,call_admitted" {
		t.Errorf("transcript call events = %v, want call_queued then call_admitted", types)
	}
}
