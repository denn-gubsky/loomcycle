package lanegate

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// fakeProvider is a driver whose stream the test drives by hand.
type fakeProvider struct {
	callErr error
	calls   atomic.Int32
	// next is the channel the next Call returns. The test owns it: it sends
	// the events and closes it.
	next chan providers.Event
}

func (f *fakeProvider) ID() string                           { return "fake" }
func (f *fakeProvider) Capabilities() providers.Capabilities { return providers.Capabilities{} }
func (f *fakeProvider) Probe(context.Context) error          { return nil }
func (f *fakeProvider) ListModels(context.Context) ([]string, error) {
	return []string{"m"}, nil
}
func (f *fakeProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	f.calls.Add(1)
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.next, nil
}

// richProvider is a driver that implements every optional interface.
type richProvider struct{ fakeProvider }

func (*richProvider) KeyEnvName() string { return "RICH_API_KEY" }
func (*richProvider) NonThinkingSibling(model string) (string, bool) {
	return model + "-plain", true
}
func (*richProvider) EnforcesToolChoice(model, _ string, _ providers.ToolChoice) bool {
	return model == "enforcing"
}
func (*richProvider) EnforcesStructuredOutput(model string, _ bool) bool {
	return model == "enforcing"
}
func (*richProvider) ModelIdentity(req providers.Request) string { return req.Model + "@8192" }

func chatLane(cfg concurrency.LaneConfig) *concurrency.CallScheduler {
	return concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"p": {Lanes: map[string]concurrency.LaneConfig{config.ModelKindChat: cfg}},
	})
}

func laneStats(t *testing.T, s *concurrency.CallScheduler) concurrency.LaneStats {
	t.Helper()
	return s.Stats()["p"].Lanes[config.ModelKindChat]
}

// waitLane polls until the lane's stats satisfy ok.
func waitLane(t *testing.T, s *concurrency.CallScheduler, what string, ok func(concurrency.LaneStats) bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if ok(laneStats(t, s)) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("lane never reached %s: %+v", what, laneStats(t, s))
}

func done() providers.Event {
	return providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}
}

// TestCall_HoldsTheSlotOnlyWhileTheStreamIsOpen — the slot is taken before the
// driver is called, a second call waits while the first one's stream is open,
// and the slot is free once the stream has ended: before the consumer is handed
// the done event, so whoever sees a call end sees its slot free.
func TestCall_HoldsTheSlotOnlyWhileTheStreamIsOpen(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	first := &fakeProvider{next: make(chan providers.Event)}
	p := Wrap("p", first, sched)

	out, err := p.Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if st := laneStats(t, sched); st.Active != 1 {
		t.Fatalf("active = %d with a stream open, want 1", st.Active)
	}

	// A second call cannot start while the first is streaming.
	second := Wrap("p", &fakeProvider{next: make(chan providers.Event)}, sched)
	started := make(chan struct{})
	go func() {
		defer close(started)
		if ch, err := second.Call(context.Background(), providers.Request{Model: "m"}); err == nil {
			close(second.inner.(*fakeProvider).next)
			for range ch {
			}
		}
	}()
	waitLane(t, sched, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if n := second.inner.(*fakeProvider).calls.Load(); n != 0 {
		t.Fatalf("the driver was called %d times while its call was still queued", n)
	}

	first.next <- providers.Event{Type: providers.EventText, Text: "hi"}
	if ev := <-out; ev.Text != "hi" {
		t.Fatalf("event = %+v, want the text", ev)
	}
	if st := laneStats(t, sched); st.Active != 1 {
		t.Fatalf("active = %d mid-stream, want 1", st.Active)
	}
	first.next <- done()
	if ev := <-out; ev.Type != providers.EventDone {
		t.Fatalf("event = %+v, want done", ev)
	}
	// The done event is in hand, so the first call's slot is already free: the
	// second call gets it and runs without the first stream having closed.
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the second call was not admitted once the first call's done event was delivered — the slot is held past the end of the call")
	}
	close(first.next)
	if _, open := <-out; open {
		t.Fatal("the wrapper's stream did not close with the driver's")
	}
	if st := laneStats(t, sched); st.Active != 0 || st.Queued != 0 {
		t.Fatalf("final lane: active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}
}

// TestCall_ReleasesTheSlotWhenTheDriverRefusesTheCall — a Call that returns an
// error never streams, so nothing else would release its slot.
func TestCall_ReleasesTheSlotWhenTheDriverRefusesTheCall(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	boom := errors.New("upstream 500")
	p := Wrap("p", &fakeProvider{callErr: boom}, sched)
	if _, err := p.Call(context.Background(), providers.Request{Model: "m"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver's", err)
	}
	if st := laneStats(t, sched); st.Active != 0 {
		t.Fatalf("active = %d after a refused call, want 0 (slot leaked)", st.Active)
	}
}

// TestCall_ReleasesTheSlotWhenCtxEndsMidStream — a consumer that left with its
// ctx cancelled reads nothing more. The wrapper must neither block on it nor
// free the slot early: it drains the driver, and the slot goes back when the
// driver has closed its stream (the host is busy until then).
func TestCall_ReleasesTheSlotWhenCtxEndsMidStream(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	inner := &fakeProvider{next: make(chan providers.Event)}
	p := Wrap("p", inner, sched)
	ctx, cancel := context.WithCancel(context.Background())
	out, err := p.Call(ctx, providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Nobody reads out. The wrapper takes this event and is stuck handing it on.
	inner.next <- providers.Event{Type: providers.EventText, Text: "unread"}
	cancel()
	// The driver has not let go yet: it still sends, and the slot is still its.
	for i := 0; i < 3; i++ {
		select {
		case inner.next <- providers.Event{Type: providers.EventText, Text: "late"}:
		case <-time.After(3 * time.Second):
			t.Fatal("the wrapper stopped draining the driver after ctx ended — the driver's goroutine would leak")
		}
	}
	if st := laneStats(t, sched); st.Active != 1 {
		t.Fatalf("active = %d while the driver is still streaming, want 1", st.Active)
	}
	close(inner.next)
	waitLane(t, sched, "the slot released", func(s concurrency.LaneStats) bool { return s.Active == 0 })
	select {
	case _, open := <-out:
		if open {
			// One event may have been handed over in the race with the cancel;
			// the stream must still close.
			if _, open = <-out; open {
				t.Fatal("the wrapper's stream stayed open after the driver closed")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the wrapper's stream never closed — its goroutine leaked")
	}
}

// TestCall_ReleasesTheSlotAtTheEndOfTheCallNotOfTheChannel — a consumer that
// returns on the error event without draining (runSummarizeCall does) leaves
// the wrapper holding the driver's trailing event with nobody to give it to.
// Its ctx is still alive — it is the run's. Were the slot released only when
// the channel closed, it would stay taken until the run ended, and on a cap of
// 1 the run's own next call would wait behind itself.
func TestCall_ReleasesTheSlotAtTheEndOfTheCallNotOfTheChannel(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	inner := &fakeProvider{next: make(chan providers.Event, 2)}
	p := Wrap("p", inner, sched)
	inner.next <- providers.Event{Type: providers.EventError, Error: "model crashed"}
	inner.next <- done() // a driver may send a trailing done after its error
	out, err := p.Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if ev := <-out; ev.Type != providers.EventError {
		t.Fatalf("event = %+v, want the error", ev)
	}
	// The consumer stops reading here.
	if st := laneStats(t, sched); st.Active != 0 {
		t.Fatalf("active = %d after the call's error was delivered, want 0", st.Active)
	}
	// The same run's next call gets the slot at once.
	next := Wrap("p", &fakeProvider{next: make(chan providers.Event)}, sched)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := next.Call(ctx, providers.Request{Model: "m"}); err != nil {
		t.Fatalf("the next call waited behind an abandoned stream: %v", err)
	}
}

// TestCall_UngatedReturnsTheDriversChannelWithoutAllocating — the common
// deployment caps nothing, and every provider is wrapped anyway. An uncapped
// call must cost nothing: the driver's own channel, no goroutine, no allocation.
func TestCall_UngatedReturnsTheDriversChannelWithoutAllocating(t *testing.T) {
	ch := make(chan providers.Event)
	inner := &fakeProvider{next: ch}
	for name, sched := range map[string]*concurrency.CallScheduler{
		"no scheduler":       nil,
		"nothing capped":     concurrency.NewCallScheduler(nil),
		"another one capped": chatLane(concurrency.LaneConfig{MaxConcurrent: 1}),
	} {
		t.Run(name, func(t *testing.T) {
			p := Wrap("uncapped", inner, sched)
			ctx := context.Background()
			req := providers.Request{Model: "m"}
			got, err := p.Call(ctx, req)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if got != (<-chan providers.Event)(ch) {
				t.Fatal("an uncapped call did not return the driver's own channel")
			}
			if allocs := testing.AllocsPerRun(200, func() { _, _ = p.Call(ctx, req) }); allocs != 0 {
				t.Fatalf("an uncapped call allocates %.1f times, want 0", allocs)
			}
		})
	}
}

// TestCall_QueueBoundRefusalIsProviderConcurrencyExhausted — a request marked
// as holding no run slot is refused, not queued, once the bound is reached, and
// the refusal is the typed error wire surfaces map to
// provider_concurrency_exhausted. The driver is never called.
func TestCall_QueueBoundRefusalIsProviderConcurrencyExhausted(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	defer release()
	inner := &fakeProvider{next: make(chan providers.Event)}
	p := Wrap("p", inner, sched)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = p.Call(WithQueueBound(ctx, 0), providers.Request{Model: "m"})
	var pce *concurrency.ErrProviderConcurrencyExhausted
	if !errors.As(err, &pce) {
		t.Fatalf("err = %v, want *ErrProviderConcurrencyExhausted (a refusal, not a wait)", err)
	}
	if pce.Provider != "p" || pce.Cap != 1 || pce.Code() != "provider_concurrency_exhausted" {
		t.Errorf("refusal = %+v code=%q, want provider=p cap=1 code=provider_concurrency_exhausted", pce, pce.Code())
	}
	if n := inner.calls.Load(); n != 0 {
		t.Errorf("the driver was called %d times for a refused request", n)
	}
	if st := laneStats(t, sched); st.Queued != 0 {
		t.Errorf("queued = %d after the refusal, want 0", st.Queued)
	}

	// Within the bound it waits like any call.
	waited := make(chan error, 1)
	go func() {
		ch, err := p.Call(WithQueueBound(context.Background(), 1), providers.Request{Model: "m"})
		if err == nil {
			close(inner.next)
			for range ch {
			}
		}
		waited <- err
	}()
	waitLane(t, sched, "one bounded call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	release()
	if err := <-waited; err != nil {
		t.Fatalf("a bounded call inside its bound: %v", err)
	}
}

// TestCall_BackgroundMarkerReachesTheLane — WithBackground is what puts
// maintenance work behind live runs; the lane only sees it if the wrapper
// passes it on.
func TestCall_BackgroundMarkerReachesTheLane(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	inner := &fakeProvider{next: make(chan providers.Event)}
	p := Wrap("p", inner, sched)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if ch, err := p.Call(WithBackground(context.Background()), providers.Request{Model: "m"}); err == nil {
			close(inner.next)
			for range ch {
			}
		}
	}()
	waitLane(t, sched, "one background call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if st := laneStats(t, sched); st.QueuedBackground != 1 {
		t.Fatalf("queued background = %d, want 1 (the marker did not reach the lane)", st.QueuedBackground)
	}
	release()
	<-finished
}

// TestCall_RunIdentityReachesTheLane — the lane takes turns across (tenant,
// user), which it can only do if the wrapper hands it the run's identity. Two
// calls of one user queue ahead of another user's one; served round-robin the
// second user goes second, served as one anonymous bucket it would go last.
func TestCall_RunIdentityReachesTheLane(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := func(run, tenant, user string, queued int) {
		inner := &fakeProvider{next: make(chan providers.Event)}
		p := Wrap("p", inner, sched)
		ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{RunID: run, TenantID: tenant, UserID: user})
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := p.Call(ctx, providers.Request{Model: "m"})
			if err != nil {
				t.Errorf("%s: %v", run, err)
				return
			}
			mu.Lock()
			order = append(order, run)
			mu.Unlock()
			close(inner.next)
			for range ch {
			}
		}()
		waitLane(t, sched, "the call queued", func(s concurrency.LaneStats) bool { return s.Queued == queued })
	}
	start("alice-1", "acme", "alice", 1)
	start("alice-2", "acme", "alice", 2)
	start("bob-1", "acme", "bob", 3)
	release()
	wg.Wait()
	if want := []string{"alice-1", "bob-1", "alice-2"}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("served in order %v, want %v (round-robin across users)", order, want)
	}
}

// TestCall_ModelIdentityIsTheDriversNotTheRequests — the cap on models in
// flight counts what the HOST holds as distinct models. A driver that knows
// better than the request's model name says so through ModelIdentity; every
// other driver's identity is the model name.
func TestCall_ModelIdentityIsTheDriversNotTheRequests(t *testing.T) {
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{"p": {MaxModelsInFlight: 1}})
	named, plain := make(chan providers.Event), make(chan providers.Event)
	for _, tc := range []struct {
		name  string
		inner providers.Provider
		next  chan providers.Event
		want  string
	}{
		{"driver names it", &richProvider{fakeProvider{next: named}}, named, "qwen@8192"},
		{"default is the model name", &fakeProvider{next: plain}, plain, "qwen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Wrap("p", tc.inner, sched).Call(context.Background(), providers.Request{Model: "qwen"})
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			inFlight := sched.Stats()["p"].Models.InFlight
			if len(inFlight) != 1 || inFlight[tc.want] != 1 {
				t.Fatalf("models in flight = %v, want one call of %q", inFlight, tc.want)
			}
			close(tc.next)
			for range out {
			}
			if left := sched.Stats()["p"].Models.InFlight; len(left) != 0 {
				t.Fatalf("models in flight after the call = %v, want none", left)
			}
		})
	}
}

// clockRecorder is a clock observer that records every report.
type clockRecorder struct {
	mu  sync.Mutex
	got []bool
}

func (c *clockRecorder) observe(held bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, held)
}

func (c *clockRecorder) reports() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.got...)
}

func sameReports(got []bool, want ...bool) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestCall_LaneWaitStopsTheClockBoundingTheRun — queueing is not working time.
// A call waiting for its lane reports held(true) to the clock on its ctx before
// the wait and held(false) after it, however it ends, so the wait does not
// count against a sub-agent's timeout_ms. It never reports to the review-hold
// observer: a walk's poll table would show a person deciding. An uncapped call
// never waits and reports nothing.
func TestCall_LaneWaitStopsTheClockBoundingTheRun(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}

	// Admitted after a wait.
	clock, holds := &clockRecorder{}, &clockRecorder{}
	ctx := teamrun.WithHoldObserver(teamrun.WithClockObserver(context.Background(), clock.observe), holds.observe)
	inner := &fakeProvider{next: make(chan providers.Event)}
	p := Wrap("p", inner, sched)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if ch, err := p.Call(ctx, providers.Request{Model: "m"}); err == nil {
			close(inner.next)
			for range ch {
			}
		}
	}()
	waitLane(t, sched, "the call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if got := clock.reports(); !sameReports(got, true) {
		t.Fatalf("clock reports while queued = %v, want [true]", got)
	}
	release()
	<-finished
	if got := clock.reports(); !sameReports(got, true, false) {
		t.Fatalf("clock reports after the wait = %v, want [true false]", got)
	}
	if got := holds.reports(); len(got) != 0 {
		t.Fatalf("the review-hold observer saw %v; a queued call is not a hold", got)
	}

	// A wait that ends by cancellation still ends the hold.
	release, err = sched.Acquire(context.Background(), "p", config.ModelKindChat, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("retake the slot: %v", err)
	}
	defer release()
	cancelled := &clockRecorder{}
	cctx, cancel := context.WithCancel(teamrun.WithClockObserver(context.Background(), cancelled.observe))
	errc := make(chan error, 1)
	go func() {
		_, err := p.Call(cctx, providers.Request{Model: "m"})
		errc <- err
	}()
	waitLane(t, sched, "the call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: err = %v, want context.Canceled", err)
	}
	if got := cancelled.reports(); !sameReports(got, true, false) {
		t.Fatalf("clock reports for a cancelled wait = %v, want [true false]", got)
	}

	// An uncapped provider never queues.
	quiet := &clockRecorder{}
	free := &fakeProvider{next: make(chan providers.Event)}
	if _, err := Wrap("uncapped", free, sched).Call(teamrun.WithClockObserver(context.Background(), quiet.observe), providers.Request{Model: "m"}); err != nil {
		t.Fatalf("uncapped call: %v", err)
	}
	if got := quiet.reports(); len(got) != 0 {
		t.Fatalf("an uncapped call reported %v to the clock, want nothing", got)
	}
}

// TestRunDone_DropsTheRunsLease — a lane keeps a run's slot reserved after a
// call so its next one lands on a warm cache. A run that has ended has no next
// call: RunDone must free the slot at once, not when the hold lapses.
func TestRunDone_DropsTheRunsLease(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1, AffinityHold: time.Hour})
	inner := &fakeProvider{next: make(chan providers.Event)}
	p := Wrap("p", inner, sched)
	ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{RunID: "run-1"})
	// The run's turn call (it carries OnEvent): only those hold a reservation.
	out, err := p.Call(ctx, providers.Request{Model: "m", OnEvent: func(providers.Event) {}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	close(inner.next)
	for range out {
	}
	if st := laneStats(t, sched); st.Active != 0 || st.Reserved != 1 {
		t.Fatalf("after the call: active=%d reserved=%d, want 0/1 (the run's lease)", st.Active, st.Reserved)
	}
	// Through the helper the loop uses, on the provider value the loop holds.
	providers.RunDone(providers.Provider(p), "another-run")
	if st := laneStats(t, sched); st.Reserved != 1 {
		t.Fatalf("another run's end dropped this run's lease: reserved=%d", st.Reserved)
	}
	providers.RunDone(providers.Provider(p), "run-1")
	if st := laneStats(t, sched); st.Reserved != 0 {
		t.Fatalf("reserved = %d after the run ended, want 0", st.Reserved)
	}
}

// TestProvider_OptionalInterfacesStayReachable — the wrapper implements none
// of the driver's optional interfaces (a direct type assertion on a registry
// provider misses all of them), and providers.As finds every one through it.
func TestProvider_OptionalInterfacesStayReachable(t *testing.T) {
	var p providers.Provider = Wrap("p", &richProvider{}, nil)

	if _, direct := p.(providers.KeyedProvider); direct {
		t.Fatal("the wrapper itself implements KeyedProvider — it must only unwrap, or a driver without the interface would appear to have it")
	}
	if kp, ok := providers.As[providers.KeyedProvider](p); !ok || kp.KeyEnvName() != "RICH_API_KEY" {
		t.Error("KeyedProvider is not reachable through the wrapper")
	}
	if dg, ok := providers.As[providers.ThinkingDowngrader](p); !ok {
		t.Error("ThinkingDowngrader is not reachable through the wrapper")
	} else if sib, _ := dg.NonThinkingSibling("m"); sib != "m-plain" {
		t.Errorf("NonThinkingSibling = %q", sib)
	}
	if _, ok := providers.As[*richProvider](p); !ok {
		t.Error("the concrete driver is not reachable through the wrapper")
	}
	forced := providers.ToolChoice{Mode: providers.ToolChoiceRequired}
	if !providers.EnforcesToolChoice(p, "enforcing", "", forced) || providers.EnforcesToolChoice(p, "other", "", forced) {
		t.Error("EnforcesToolChoice did not use the driver's per-model answer")
	}
	if !providers.EnforcesStructuredOutput(p, "enforcing", false) || providers.EnforcesStructuredOutput(p, "other", false) {
		t.Error("EnforcesStructuredOutput did not use the driver's per-model answer")
	}
	if got := providers.ModelIdentity(p, providers.Request{Model: "m"}); got != "m@8192" {
		t.Errorf("ModelIdentity = %q, want the driver's", got)
	}
	// A driver with none of them has none of them through the wrapper either.
	var plain providers.Provider = Wrap("p", &fakeProvider{}, nil)
	if _, ok := providers.As[providers.KeyedProvider](plain); ok {
		t.Error("a driver without KeyedProvider gained it by being wrapped")
	}
}

// TestCall_KeepsTheConsumersBackPressure — the wrapper adds no buffer: a driver
// that blocks on a slow consumer still blocks on it.
func TestCall_KeepsTheConsumersBackPressure(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	inner := &fakeProvider{next: make(chan providers.Event)}
	out, err := Wrap("p", inner, sched).Call(context.Background(), providers.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var sent atomic.Int32
	go func() {
		defer close(inner.next)
		for i := 0; i < 5; i++ {
			inner.next <- providers.Event{Type: providers.EventText, Text: "x"}
			sent.Add(1)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	// One event is in the wrapper's hand; the driver is blocked on the second.
	if n := sent.Load(); n > 1 {
		t.Fatalf("the driver sent %d events with nobody reading, want at most 1 (the wrapper buffers)", n)
	}
	n := 0
	for range out {
		n++
	}
	if n != 5 {
		t.Fatalf("consumer received %d events, want 5", n)
	}
}

// TestSchedulerConfig_FoldsTheShorthandIntoTheChatLane — max_concurrent is the
// chat lane's cap, a lane block carries its own policy, and a provider that
// caps nothing has no entry (so its calls stay ungated).
func TestSchedulerConfig_FoldsTheShorthandIntoTheChatLane(t *testing.T) {
	got := SchedulerConfig(map[string]config.ProviderConfig{
		"plain":     {},
		"shorthand": {MaxConcurrent: 2},
		"laned": {
			MaxModelsInFlight: 1,
			Lanes: map[string]config.ProviderLane{
				config.ModelKindChat:     {MaxConcurrent: 3, QuantumCalls: 4, AffinityHoldMS: 1500, BackgroundShare: 0.25},
				config.ModelKindDecision: {MaxConcurrent: 1},
			},
		},
	}, nil)
	if _, ok := got["plain"]; ok {
		t.Error("a provider that caps nothing got a scheduler entry")
	}
	if l := got["shorthand"].Lanes[config.ModelKindChat]; l.MaxConcurrent != 2 {
		t.Errorf("shorthand chat lane = %+v, want max_concurrent 2", l)
	}
	laned := got["laned"]
	want := concurrency.LaneConfig{MaxConcurrent: 3, QuantumCalls: 4, AffinityHold: 1500 * time.Millisecond, BackgroundShare: 0.25}
	if !reflect.DeepEqual(laned.Lanes[config.ModelKindChat], want) {
		t.Errorf("chat lane = %+v, want %+v", laned.Lanes[config.ModelKindChat], want)
	}
	if laned.Lanes[config.ModelKindDecision].MaxConcurrent != 1 || laned.MaxModelsInFlight != 1 {
		t.Errorf("laned = %+v, want a decision lane of 1 and one model in flight", laned)
	}
	if laned.ModelSwitchMaxWait != 30*time.Second {
		t.Errorf("model switch wait = %s, want the 30s default", laned.ModelSwitchMaxWait)
	}
}

// TestSchedulerConfig_ResolvesModelCapsToConcreteModels — a lane's model_caps
// reach the scheduler keyed by the concrete model, an alias resolved through
// models:, so the lane matches the identity a driver reports (ollama's
// model@num_ctx starts with the concrete name, never the alias). The shorthand
// chat lane keeps its caps, and a lane without caps carries none.
func TestSchedulerConfig_ResolvesModelCapsToConcreteModels(t *testing.T) {
	models := map[string]config.ModelRef{"ornith": {Provider: "ollama-local", Model: "ornith-1.5:35b"}}
	got := SchedulerConfig(map[string]config.ProviderConfig{
		"ollama-local": {
			MaxConcurrent: 4,
			Lanes: map[string]config.ProviderLane{
				config.ModelKindChat:     {ModelCaps: map[string]int{"qwen3.6:latest": 1, "ornith": 1}},
				config.ModelKindDecision: {MaxConcurrent: 2},
			},
		},
	}, models)
	lanes := got["ollama-local"].Lanes
	want := map[string]int{"qwen3.6:latest": 1, "ornith-1.5:35b": 1}
	if l := lanes[config.ModelKindChat]; l.MaxConcurrent != 4 || !reflect.DeepEqual(l.ModelCaps, want) {
		t.Errorf("chat lane = %+v, want max_concurrent 4 and model caps %v", l, want)
	}
	if caps := lanes[config.ModelKindDecision].ModelCaps; caps != nil {
		t.Errorf("decision lane model caps = %v, want none", caps)
	}

	// End to end through the scheduler: the alias's model at a context size
	// is held at one call while another model takes a free slot.
	s := concurrency.NewCallScheduler(got)
	ctx := context.Background()
	rel, err := s.Acquire(ctx, "ollama-local", config.ModelKindChat, concurrency.CallInfo{Model: "ornith-1.5:35b@65536"})
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	if _, err := s.AcquireBounded(ctx, "ollama-local", config.ModelKindChat, concurrency.CallInfo{Model: "ornith-1.5:35b@65536"}, 0); !concurrency.IsQueueFull(err) {
		t.Errorf("a second call to the alias's model = %v, want it held back (queue full at bound 0)", err)
	}
	other, err := s.AcquireBounded(ctx, "ollama-local", config.ModelKindChat, concurrency.CallInfo{Model: "gemma4:latest@65536"}, 0)
	if err != nil {
		t.Fatalf("a call to another model = %v, want a free slot", err)
	}
	other()
}

// The exempt kinds reach the scheduler, so an embedding call on a provider with
// max_models_in_flight: 1 does not wait for the chat model.
func TestSchedulerConfig_CarriesTheModelCapsExemptKinds(t *testing.T) {
	got := SchedulerConfig(map[string]config.ProviderConfig{
		"p": {MaxModelsInFlight: 1, ModelsInFlightExemptKinds: []string{config.ModelKindEmbedder}},
	}, nil)
	if ex := got["p"].ExemptKinds; len(ex) != 1 || ex[0] != config.ModelKindEmbedder {
		t.Fatalf("exempt kinds = %v, want [embedder]", ex)
	}
	s := concurrency.NewCallScheduler(got)
	if s.Gated("p", config.ModelKindEmbedder) || !s.Gated("p", config.ModelKindChat) {
		t.Errorf("Gated(embedder)=%v Gated(chat)=%v, want embeddings ungated and chat under the model cap",
			s.Gated("p", config.ModelKindEmbedder), s.Gated("p", config.ModelKindChat))
	}
}

// eventLog records what a call reported on req.OnEvent.
type eventLog struct {
	mu  sync.Mutex
	evs []providers.Event
}

func (l *eventLog) on(ev providers.Event) {
	l.mu.Lock()
	l.evs = append(l.evs, ev)
	l.mu.Unlock()
}

func (l *eventLog) list() []providers.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]providers.Event(nil), l.evs...)
}

// TestCall_ReportsItsWaitOnTheRequestsEventHook — a call that has to wait for
// its lane reports call_queued (why, where, which provider and kind) before it
// blocks, and call_admitted with its wait once it has the slot, on the hook the
// loop gives every call. A call that gets its slot at once reports nothing.
func TestCall_ReportsItsWaitOnTheRequestsEventHook(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	first := &fakeProvider{next: make(chan providers.Event)}
	var firstLog eventLog
	out, err := Wrap("p", first, sched).Call(context.Background(), providers.Request{Model: "m", OnEvent: firstLog.on})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if evs := firstLog.list(); len(evs) != 0 {
		t.Fatalf("a call granted at once reported %+v", evs)
	}

	second := &fakeProvider{next: make(chan providers.Event)}
	var log eventLog
	admitted := make(chan error, 1)
	go func() {
		ch, err := Wrap("p", second, sched).Call(context.Background(), providers.Request{Model: "m", OnEvent: log.on})
		admitted <- err
		if err == nil {
			for range ch {
			}
		}
	}()
	waitLane(t, sched, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	evs := log.list()
	if len(evs) != 1 || evs[0].Type != providers.EventCallQueued {
		t.Fatalf("while waiting the call reported %+v, want one call_queued", evs)
	}
	if a := evs[0].Admission; a == nil || *a != (providers.AdmissionInfo{Reason: "lane", Position: 1, Provider: "p", Kind: config.ModelKindChat}) {
		t.Fatalf("call_queued payload = %+v, want lane, position 1, provider p, kind chat", a)
	}

	time.Sleep(20 * time.Millisecond) // a wait the admission can measure
	first.next <- done()
	<-out
	if err := <-admitted; err != nil {
		t.Fatalf("second Call: %v", err)
	}
	evs = log.list()
	if len(evs) != 2 || evs[1].Type != providers.EventCallAdmitted {
		t.Fatalf("after the slot freed the call reported %+v, want call_queued then call_admitted", evs)
	}
	if a := evs[1].Admission; a == nil || a.Provider != "p" || a.Kind != config.ModelKindChat || a.WaitMs < 20 {
		t.Fatalf("call_admitted payload = %+v, want provider p, kind chat, wait_ms >= 20", a)
	}
	close(second.next)
	close(first.next)
}

// TestCall_ReportsNoAdmissionWhenTheWaitIsCancelled — a call whose ctx ends
// while it waits never got a slot, so it reports no admission.
func TestCall_ReportsNoAdmissionWhenTheWaitIsCancelled(t *testing.T) {
	sched := chatLane(concurrency.LaneConfig{MaxConcurrent: 1})
	first := &fakeProvider{next: make(chan providers.Event)}
	if _, err := Wrap("p", first, sched).Call(context.Background(), providers.Request{Model: "m"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var log eventLog
	failed := make(chan error, 1)
	go func() {
		_, err := Wrap("p", &fakeProvider{}, sched).Call(ctx, providers.Request{Model: "m", OnEvent: log.on})
		failed <- err
	}()
	waitLane(t, sched, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	cancel()
	if err := <-failed; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: err %v, want context.Canceled", err)
	}
	if evs := log.list(); len(evs) != 1 || evs[0].Type != providers.EventCallQueued {
		t.Fatalf("a cancelled wait reported %+v, want only call_queued", evs)
	}
	first.next <- done()
	close(first.next)
}
