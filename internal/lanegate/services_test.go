package lanegate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// kindLane is a scheduler whose only lane is provider p's lane of one kind.
func kindLane(kind string, cfg concurrency.LaneConfig) *concurrency.CallScheduler {
	return concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{
		"p": {Lanes: map[string]concurrency.LaneConfig{kind: cfg}},
	})
}

func kindStats(s *concurrency.CallScheduler, kind string) concurrency.LaneStats {
	return s.Stats()["p"].Lanes[kind]
}

// recv returns the call's result, failing the test instead of hanging it when
// the call never returns — which is what a slot that is never given back looks
// like from here.
func recv(t *testing.T, what string, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never returned", what)
		return nil
	}
}

func waitKind(t *testing.T, s *concurrency.CallScheduler, kind, what string, ok func(concurrency.LaneStats) bool) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if ok(kindStats(s, kind)) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the %s lane never reached %s: %+v", kind, what, kindStats(s, kind))
}

// fakeDecision is a decision driver whose call the test holds open. gate nil =
// answer at once.
type fakeDecision struct {
	calls atomic.Int32
	gate  chan struct{}
	err   error
}

func (f *fakeDecision) Limits(string) decision.Limits {
	return decision.Limits{MaxQuestions: 4, MinOptions: 2, MaxOptions: 4}
}

func (f *fakeDecision) Decide(ctx context.Context, req decision.Request) (*decision.Response, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &decision.Response{Provider: "p", Model: req.Model}, nil
}

var yesNo = decision.Request{Model: "nimble", Questions: map[string]decision.Question{
	"q": {Type: decision.TypeNoul, Instructions: "Is it?"},
}}

// fakeEmbedder is an embedder whose call the test holds open.
type fakeEmbedder struct {
	calls atomic.Int32
	gate  chan struct{}
	err   error
	model string
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return make([][]float32, len(texts)), nil
}
func (f *fakeEmbedder) Model() string {
	if f.model != "" {
		return f.model
	}
	return "embed-m"
}
func (f *fakeEmbedder) Provider() string { return "fake" }
func (f *fakeEmbedder) Dimension() int   { return 3 }

// TestDecision_HoldsItsLaneSlotOnlyForTheCall — a decision takes its
// provider's decision lane before the driver is asked and gives it back when
// the driver returns; a second one waits meanwhile and is not sent.
func TestDecision_HoldsItsLaneSlotOnlyForTheCall(t *testing.T) {
	sched := kindLane(config.ModelKindDecision, concurrency.LaneConfig{MaxConcurrent: 1})
	first := &fakeDecision{gate: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		_, err := WrapDecision("p", first, sched).Decide(context.Background(), yesNo)
		firstDone <- err
	}()
	waitKind(t, sched, config.ModelKindDecision, "one call in flight", func(s concurrency.LaneStats) bool { return s.Active == 1 })

	second := &fakeDecision{}
	secondDone := make(chan error, 1)
	go func() {
		_, err := WrapDecision("p", second, sched).Decide(context.Background(), yesNo)
		secondDone <- err
	}()
	waitKind(t, sched, config.ModelKindDecision, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if n := second.calls.Load(); n != 0 {
		t.Fatalf("the driver was asked %d times while its call was still queued", n)
	}

	close(first.gate)
	if err := recv(t, "the first call", firstDone); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := recv(t, "the queued call", secondDone); err != nil {
		t.Fatalf("second: %v", err)
	}
	if st := kindStats(sched, config.ModelKindDecision); st.Active != 0 || st.Queued != 0 || st.Grants != 2 {
		t.Fatalf("decision lane = %+v, want empty after 2 grants", st)
	}
	// It is the DECISION lane that was taken, and no other.
	if st := kindStats(sched, config.ModelKindChat); st.Grants != 0 {
		t.Fatalf("a decision took the chat lane: %+v", st)
	}
}

// TestDecision_ReleasesTheSlotOnErrorAndOnCtxCancel — the slot comes back
// whatever ends the call: the driver's error, the caller's ctx ending the call,
// or the caller's ctx ending the WAIT (which is a decision fault carrying the
// ctx error, and never reaches the driver).
func TestDecision_ReleasesTheSlotOnErrorAndOnCtxCancel(t *testing.T) {
	sched := kindLane(config.ModelKindDecision, concurrency.LaneConfig{MaxConcurrent: 1})
	free := func(when string) {
		t.Helper()
		if st := kindStats(sched, config.ModelKindDecision); st.Active != 0 || st.Queued != 0 {
			t.Fatalf("%s: decision lane = %+v, want the slot back", when, st)
		}
	}

	boom := errors.New("boom")
	if _, err := WrapDecision("p", &fakeDecision{err: boom}, sched).Decide(context.Background(), yesNo); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver's", err)
	}
	free("after the driver's error")

	// ctx ends the call in flight.
	ctx, cancel := context.WithCancel(context.Background())
	hung := &fakeDecision{gate: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := WrapDecision("p", hung, sched).Decide(ctx, yesNo)
		done <- err
	}()
	waitKind(t, sched, config.ModelKindDecision, "one call in flight", func(s concurrency.LaneStats) bool { return s.Active == 1 })
	cancel()
	if err := recv(t, "the cancelled call", done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: err = %v, want context.Canceled", err)
	}
	free("after a cancelled call")

	// ctx ends the wait.
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindDecision, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	waiting := &fakeDecision{}
	wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer wcancel()
	_, err = WrapDecision("p", waiting, sched).Decide(wctx, yesNo)
	if decision.CodeOf(err) != decision.CodeTimeout || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a wait the caller's deadline ended: err = %v, want a decision timeout wrapping DeadlineExceeded", err)
	}
	if n := waiting.calls.Load(); n != 0 {
		t.Fatalf("the driver was asked %d times by a call that never got a slot", n)
	}
	release()
	free("after a cancelled wait")
}

// TestDecision_AnInvalidRequestIsRefusedWithoutQueueing — a request the driver
// would refuse unasked does not wait for a slot to be told so.
func TestDecision_AnInvalidRequestIsRefusedWithoutQueueing(t *testing.T) {
	sched := kindLane(config.ModelKindDecision, concurrency.LaneConfig{MaxConcurrent: 1})
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindDecision, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	defer release()
	inner := &fakeDecision{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = WrapDecision("p", inner, sched).Decide(ctx, decision.Request{Questions: yesNo.Questions}) // no model
	if decision.CodeOf(err) != decision.CodeModelNotFound {
		t.Fatalf("err = %v, want the request refused as naming no model", err)
	}
	if st := kindStats(sched, config.ModelKindDecision); st.Queued != 0 {
		t.Fatalf("an invalid request queued: %+v", st)
	}
}

// TestEmbedder_WaitsForTheEmbedderLane — an embedding takes its provider's
// embedder lane for the call: a second one waits, unsent, until the first
// returns.
func TestEmbedder_WaitsForTheEmbedderLane(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	first := &fakeEmbedder{gate: make(chan struct{})}
	firstDone := make(chan error, 1)
	go func() {
		_, err := WrapEmbedder("p", first, sched).Embed(context.Background(), []string{"a"})
		firstDone <- err
	}()
	waitKind(t, sched, config.ModelKindEmbedder, "one call in flight", func(s concurrency.LaneStats) bool { return s.Active == 1 })

	second := &fakeEmbedder{}
	secondDone := make(chan error, 1)
	go func() {
		vecs, err := WrapEmbedder("p", second, sched).Embed(context.Background(), []string{"a", "b"})
		if err == nil && len(vecs) != 2 {
			err = errors.New("the vectors did not come back")
		}
		secondDone <- err
	}()
	waitKind(t, sched, config.ModelKindEmbedder, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	if n := second.calls.Load(); n != 0 {
		t.Fatalf("the embedder was called %d times while its call was still queued", n)
	}
	close(first.gate)
	if err := recv(t, "the first call", firstDone); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := recv(t, "the queued call", secondDone); err != nil {
		t.Fatalf("second: %v", err)
	}
	if st := kindStats(sched, config.ModelKindEmbedder); st.Active != 0 || st.Queued != 0 || st.Grants != 2 {
		t.Fatalf("embedder lane = %+v, want empty after 2 grants", st)
	}
}

// TestEmbedder_ReleasesTheSlotOnErrorAndOnCtxCancel — as for a decision.
func TestEmbedder_ReleasesTheSlotOnErrorAndOnCtxCancel(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	free := func(when string) {
		t.Helper()
		if st := kindStats(sched, config.ModelKindEmbedder); st.Active != 0 || st.Queued != 0 {
			t.Fatalf("%s: embedder lane = %+v, want the slot back", when, st)
		}
	}
	boom := errors.New("boom")
	if _, err := WrapEmbedder("p", &fakeEmbedder{err: boom}, sched).Embed(context.Background(), []string{"a"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the embedder's", err)
	}
	free("after the embedder's error")

	ctx, cancel := context.WithCancel(context.Background())
	hung := &fakeEmbedder{gate: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := WrapEmbedder("p", hung, sched).Embed(ctx, []string{"a"})
		done <- err
	}()
	waitKind(t, sched, config.ModelKindEmbedder, "one call in flight", func(s concurrency.LaneStats) bool { return s.Active == 1 })
	cancel()
	if err := recv(t, "the cancelled call", done); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call: err = %v, want context.Canceled", err)
	}
	free("after a cancelled call")

	release, err := sched.Acquire(context.Background(), "p", config.ModelKindEmbedder, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	waiting := &fakeEmbedder{}
	wctx, wcancel := context.WithCancel(context.Background())
	werr := make(chan error, 1)
	go func() {
		_, err := WrapEmbedder("p", waiting, sched).Embed(wctx, []string{"a"})
		werr <- err
	}()
	waitKind(t, sched, config.ModelKindEmbedder, "one call queued", func(s concurrency.LaneStats) bool { return s.Queued == 1 })
	wcancel()
	if err := recv(t, "the cancelled wait", werr); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: err = %v, want context.Canceled", err)
	}
	if n := waiting.calls.Load(); n != 0 {
		t.Fatalf("the embedder was called %d times by a call that never got a slot", n)
	}
	release()
	free("after a cancelled wait")
}

// TestEmbedder_KeepsEverythingElseAnEmbedderIs — the wrapper changes Embed and
// nothing else: the three descriptive methods are the driver's, the driver is
// still reachable, a nil embedder stays nil (callers test for it), and the
// health observer placed around it — the one optional interface an embedder is
// asserted to — still reports.
func TestEmbedder_KeepsEverythingElseAnEmbedderIs(t *testing.T) {
	inner := &fakeEmbedder{model: "mxbai"}
	e := WrapEmbedder("p", inner, nil)
	if e.Model() != "mxbai" || e.Provider() != "fake" || e.Dimension() != 3 {
		t.Fatalf("wrapped = %s/%s/%d, want the driver's fake/mxbai/3", e.Provider(), e.Model(), e.Dimension())
	}
	if got := e.(*Embedder).Unwrap(); got != providers.Embedder(inner) {
		t.Fatalf("Unwrap = %v, want the driver", got)
	}
	if got := WrapEmbedder("p", nil, nil); got != nil {
		t.Fatalf("WrapEmbedder(nil) = %#v, want a nil interface — `embedder == nil` must still mean none is configured", got)
	}
	// The composition the server builds: the observer outermost.
	var observed providers.Embedder = providers.ObserveEmbedder(e)
	reporter, ok := observed.(interface {
		Health() providers.EmbedderHealth
	})
	if !ok {
		t.Fatal("the observed embedder no longer reports its health")
	}
	if _, err := observed.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if h := reporter.Health(); h.Calls != 1 || h.State != providers.EmbedderOK {
		t.Fatalf("health = %+v, want one ok call seen through the lane wrapper", h)
	}
	// Ungated (nil scheduler): the driver is called directly.
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("the driver saw %d calls, want 1", n)
	}
}

// TestServiceCalls_RunIdentityAndClockReachTheLane — a decision and an
// embedding made from inside a run queue AS that run: the lane takes turns
// across (tenant, user) by the RunMeta on the ctx (a tool's ctx inherits the
// loop's), and the wait stops the clock bounding the run. Two calls of one user
// queue ahead of another user's one; round-robin serves the other user second.
func TestServiceCalls_RunIdentityAndClockReachTheLane(t *testing.T) {
	for _, kind := range []string{config.ModelKindDecision, config.ModelKindEmbedder} {
		t.Run(kind, func(t *testing.T) {
			sched := kindLane(kind, concurrency.LaneConfig{MaxConcurrent: 1})
			release, err := sched.Acquire(context.Background(), "p", kind, concurrency.CallInfo{})
			if err != nil {
				t.Fatalf("take the slot: %v", err)
			}
			var mu sync.Mutex
			var order []string
			var wg sync.WaitGroup
			clock := &clockRecorder{}
			start := func(run, user string, queued int) {
				ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{RunID: run, TenantID: "acme", UserID: user})
				if run == "alice-1" {
					ctx = teamrun.WithClockObserver(ctx, clock.observe)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Recorded inside the slot: the lane is 1 wide, so the order
					// of these appends is the order the lane served them in.
					ctx := context.WithValue(ctx, recordKey{}, func() {
						mu.Lock()
						order = append(order, run)
						mu.Unlock()
					})
					if err := callRecording(ctx, kind, sched); err != nil {
						t.Errorf("%s: %v", run, err)
					}
				}()
				waitKind(t, sched, kind, "the call queued", func(s concurrency.LaneStats) bool { return s.Queued == queued })
			}
			start("alice-1", "alice", 1)
			start("alice-2", "alice", 2)
			start("bob-1", "bob", 3)
			if got := clock.reports(); !sameReports(got, true) {
				t.Fatalf("clock reports while queued = %v, want [true]", got)
			}
			release()
			wg.Wait()
			if want := []string{"alice-1", "bob-1", "alice-2"}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
				t.Fatalf("served in order %v, want %v (round-robin across users)", order, want)
			}
			if got := clock.reports(); !sameReports(got, true, false) {
				t.Fatalf("clock reports after the wait = %v, want [true false]", got)
			}
		})
	}
}

type recordKey struct{}

// recordingDecision and recordingEmbedder run the func on the ctx while they
// hold the slot.
type recordingDecision struct{ fakeDecision }

func (r *recordingDecision) Decide(ctx context.Context, req decision.Request) (*decision.Response, error) {
	ctx.Value(recordKey{}).(func())()
	return r.fakeDecision.Decide(ctx, req)
}

type recordingEmbedder struct{ fakeEmbedder }

func (r *recordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	ctx.Value(recordKey{}).(func())()
	return r.fakeEmbedder.Embed(ctx, texts)
}

func callRecording(ctx context.Context, kind string, sched *concurrency.CallScheduler) error {
	if kind == config.ModelKindDecision {
		_, err := WrapDecision("p", &recordingDecision{}, sched).Decide(ctx, yesNo)
		return err
	}
	_, err := WrapEmbedder("p", &recordingEmbedder{}, sched).Embed(ctx, []string{"a"})
	return err
}

// TestServiceCalls_ABackgroundCallIsServedAfterAForegroundOne — maintenance
// work marked WithBackground waits behind a live call on the same lane even
// when it queued first.
func TestServiceCalls_ABackgroundCallIsServedAfterAForegroundOne(t *testing.T) {
	for _, kind := range []string{config.ModelKindDecision, config.ModelKindEmbedder} {
		t.Run(kind, func(t *testing.T) {
			sched := kindLane(kind, concurrency.LaneConfig{MaxConcurrent: 1})
			release, err := sched.Acquire(context.Background(), "p", kind, concurrency.CallInfo{})
			if err != nil {
				t.Fatalf("take the slot: %v", err)
			}
			var mu sync.Mutex
			var order []string
			var wg sync.WaitGroup
			start := func(name string, ctx context.Context, queued, queuedBackground int) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx := context.WithValue(ctx, recordKey{}, func() {
						mu.Lock()
						order = append(order, name)
						mu.Unlock()
					})
					if err := callRecording(ctx, kind, sched); err != nil {
						t.Errorf("%s: %v", name, err)
					}
				}()
				waitKind(t, sched, kind, name+" queued", func(s concurrency.LaneStats) bool {
					return s.Queued == queued && s.QueuedBackground == queuedBackground
				})
			}
			start("background", WithBackground(context.Background()), 1, 1)
			start("foreground", context.Background(), 2, 1)
			release()
			wg.Wait()
			if len(order) != 2 || order[0] != "foreground" || order[1] != "background" {
				t.Fatalf("served in order %v, want the foreground call first though it queued second", order)
			}
		})
	}
}

// TestServiceCalls_QueueBoundRefusalIsProviderConcurrencyExhausted — a request
// marked as holding no run slot is refused, not queued, once the bound is
// reached, on these lanes as on the chat lane.
func TestServiceCalls_QueueBoundRefusalIsProviderConcurrencyExhausted(t *testing.T) {
	sched := kindLane(config.ModelKindEmbedder, concurrency.LaneConfig{MaxConcurrent: 1})
	release, err := sched.Acquire(context.Background(), "p", config.ModelKindEmbedder, concurrency.CallInfo{})
	if err != nil {
		t.Fatalf("take the slot: %v", err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	inner := &fakeEmbedder{}
	_, err = WrapEmbedder("p", inner, sched).Embed(WithQueueBound(ctx, 0), []string{"a"})
	var pce *concurrency.ErrProviderConcurrencyExhausted
	if !errors.As(err, &pce) || pce.Provider != "p" || pce.Cap != 1 {
		t.Fatalf("err = %v, want *ErrProviderConcurrencyExhausted for provider p, cap 1", err)
	}
	if n := inner.calls.Load(); n != 0 {
		t.Fatalf("the embedder was called %d times for a refused request", n)
	}
}

// TestEmbedder_WaitsWhileAChatModelComputes — max_models_in_flight is the
// provider's, not a lane's: with it at 1 and NO embedder lane, an embedding
// still waits while a chat model computes, because the host can hold one model
// at a time. End to end through the wrappers a chat call and an embedding
// actually go through; the embedding is sent only once the chat stream is over.
func TestEmbedder_WaitsWhileAChatModelComputes(t *testing.T) {
	sched := concurrency.NewCallScheduler(map[string]concurrency.ProviderLanesConfig{"p": {MaxModelsInFlight: 1}})
	chat := &fakeProvider{next: make(chan providers.Event)}
	out, err := Wrap("p", chat, sched).Call(context.Background(), providers.Request{Model: "chat-m"})
	if err != nil {
		t.Fatalf("chat call: %v", err)
	}

	emb := &fakeEmbedder{model: "embed-m"}
	embedded := make(chan error, 1)
	go func() {
		_, err := WrapEmbedder("p", emb, sched).Embed(context.Background(), []string{"a"})
		embedded <- err
	}()
	end := time.Now().Add(3 * time.Second)
	for sched.Stats()["p"].Models.Waiting != 1 {
		if time.Now().After(end) {
			t.Fatalf("the embedding never queued for its model: %+v", sched.Stats()["p"].Models)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n := emb.calls.Load(); n != 0 {
		t.Fatalf("the embedder was called %d times while a chat model computed", n)
	}

	chat.next <- done()
	if ev := <-out; ev.Type != providers.EventDone {
		t.Fatalf("event = %+v, want done", ev)
	}
	close(chat.next)
	for range out {
	}
	if err := recv(t, "the embedding", embedded); err != nil {
		t.Fatalf("the embedding after the chat call: %v", err)
	}
	if n := emb.calls.Load(); n != 1 {
		t.Fatalf("the embedder was called %d times, want 1", n)
	}
}
