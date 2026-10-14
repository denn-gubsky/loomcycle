package concurrency

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// schedNow acquires on a scheduler that must grant at once.
func schedNow(t *testing.T, s *CallScheduler, provider, kind string, c CallInfo) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), schedWait)
	defer cancel()
	rel, err := s.Acquire(ctx, provider, kind, c)
	if err != nil {
		t.Fatalf("Acquire(%s, %s, %+v) = %v, want an immediate grant", provider, kind, c, err)
	}
	return rel
}

// schedAsync starts an acquire that is expected to wait.
func schedAsync(t *testing.T, s *CallScheduler, provider, kind string, c CallInfo) <-chan func() {
	t.Helper()
	return acquireAsync(t, schedCtx(t), func(ctx context.Context) (func(), error) {
		return s.Acquire(ctx, provider, kind, c)
	})
}

// oneLane is a scheduler config with a single chat lane on provider "p".
func oneLane(lc LaneConfig, maxModels int) map[string]ProviderLanesConfig {
	return map[string]ProviderLanesConfig{
		"p": {Lanes: map[string]LaneConfig{"chat": lc}, MaxModelsInFlight: maxModels},
	}
}

func TestCallScheduler_UngatedPathIsNoopWithoutAllocating(t *testing.T) {
	ctx := context.Background()
	// No models-in-flight cap: with one, a kind that has no lane is still
	// counted at the model gate (TestCallScheduler_KindWithoutLaneStillWaitsForItsModel).
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 1}, 0))
	var nilSched *CallScheduler
	c := CallInfo{Owner: "run", TenantID: "t", UserID: "u", Model: "m"}
	allocs := testing.AllocsPerRun(200, func() {
		r1, _ := nilSched.Acquire(ctx, "p", "chat", c) // no scheduler
		r2, _ := s.Acquire(ctx, "other", "chat", c)    // unknown provider
		r3, _ := s.Acquire(ctx, "p", "embedder", c)    // kind with no lane
		nilSched.Done("run")
		r1()
		r2()
		r3()
	})
	if allocs != 0 {
		t.Fatalf("allocs per ungated Acquire = %v, want 0", allocs)
	}
	if st := s.Stats()["p"]; st.Lanes["chat"].Active != 0 || st.Lanes["chat"].Grants != 0 || st.Models.InFlight != nil {
		t.Fatalf("stats = %+v, want the ungated calls to have touched nothing", st)
	}
}

func TestCallScheduler_NilSchedulerGatesNothing(t *testing.T) {
	var s *CallScheduler
	for i := 0; i < 10; i++ {
		schedNow(t, s, "p", "chat", CallInfo{Model: fmt.Sprintf("m%d", i)})
	}
	s.Done("run")
	s.Reload(oneLane(LaneConfig{MaxConcurrent: 1}, 1))
	if st := s.Stats(); st != nil {
		t.Fatalf("Stats() = %v, want nil", st)
	}
}

func TestCallScheduler_LaneWithoutSlotsIsUngated(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 0}, 0))
	for i := 0; i < 5; i++ {
		schedNow(t, s, "p", "chat", CallInfo{Model: fmt.Sprintf("m%d", i)})
	}
	if st := s.Stats(); st != nil {
		t.Fatalf("Stats() = %v, want nil: a provider with no cap of either kind builds nothing", st)
	}
}

func TestCallScheduler_CapsCallsPerProviderAndKind(t *testing.T) {
	s := NewCallScheduler(map[string]ProviderLanesConfig{
		"p": {Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 1}, "embedder": {MaxConcurrent: 1}}},
		"q": {Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 1}}},
	})
	rel := schedNow(t, s, "p", "chat", CallInfo{})
	// A full p/chat lane holds back neither p's other kind nor another provider.
	schedNow(t, s, "p", "embedder", CallInfo{})
	schedNow(t, s, "q", "chat", CallInfo{})
	second := schedAsync(t, s, "p", "chat", CallInfo{})
	eventually(t, "second chat call queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 1 })
	mustStillWait(t, second, "a second call on a one-slot lane")
	rel()
	mustGrant(t, second, "the queued call after a release")
}

func TestCallScheduler_EntersModelGateBeforeLane(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 2}, 1))
	relX := schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	y := schedAsync(t, s, "p", "chat", CallInfo{Model: "y"})
	eventually(t, "y waiting at the gate", func() bool { return s.Stats()["p"].Models.Waiting == 1 })
	mustStillWait(t, y, "a second model")
	// y waits at the gate holding no lane slot: the lane still has room for x.
	if st := s.Stats()["p"].Lanes["chat"]; st.Active != 1 || st.Queued != 0 {
		t.Fatalf("lane stats = %+v, want y to hold no lane slot while it waits for its model", st)
	}
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})()
	relX()
	mustGrant(t, y, "the second model once the first drained")
}

// The models-in-flight cap covers every call to the provider: a kind with no
// lane (an embedder beside a capped chat lane) must still wait for its model,
// or it would start computing next to the chat model the cap exists to protect.
func TestCallScheduler_KindWithoutLaneStillWaitsForItsModel(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 2}, 1))
	relChat := schedNow(t, s, "p", "chat", CallInfo{Model: "chat-model"})
	emb := schedAsync(t, s, "p", "embedder", CallInfo{Model: "embed-model"})
	eventually(t, "the embed call waiting at the gate", func() bool { return s.Stats()["p"].Models.Waiting == 1 })
	mustStillWait(t, emb, "an embed call while another model computes")
	relChat()
	mustGrant(t, emb, "the embed call once the chat model drained")
}

// An exempt kind is left out of the models-in-flight cap entirely: its call
// neither waits for the model in flight nor counts as one, so a call to
// another model does not wait for it. Its own lane still caps it.
func TestCallScheduler_ExemptKindBypassesTheModelCapButNotItsLane(t *testing.T) {
	s := NewCallScheduler(map[string]ProviderLanesConfig{"p": {
		Lanes:             map[string]LaneConfig{"chat": {MaxConcurrent: 2}, "embedder": {MaxConcurrent: 1}},
		MaxModelsInFlight: 1,
		ExemptKinds:       []string{"embedder"},
	}})
	relChat := schedNow(t, s, "p", "chat", CallInfo{Model: "chat-model"})
	relEmb := schedNow(t, s, "p", "embedder", CallInfo{Model: "embed-model"})
	if in := s.Stats()["p"].Models.InFlight; len(in) != 1 || in["chat-model"] == 0 {
		t.Fatalf("models in flight = %v, want only the chat model counted", in)
	}
	second := schedAsync(t, s, "p", "embedder", CallInfo{Model: "embed-model"})
	eventually(t, "the second embed call queued in its lane", func() bool { return s.Stats()["p"].Lanes["embedder"].Queued == 1 })
	mustStillWait(t, second, "a second embed call on a one-slot embedder lane")
	relEmb()
	mustGrant(t, second, "the queued embed call after a release")

	// A chat call to another model waits for the chat model only.
	relChat()
	schedNow(t, s, "p", "chat", CallInfo{Model: "other-chat-model"})()

	// With no embedder lane, an exempt embed call is not gated at all.
	s.Reload(map[string]ProviderLanesConfig{"p": {
		Lanes:             map[string]LaneConfig{"chat": {MaxConcurrent: 2}},
		MaxModelsInFlight: 1,
		ExemptKinds:       []string{"embedder"},
	}})
	if s.Gated("p", "embedder") || !s.Gated("p", "decision") {
		t.Fatalf("Gated(embedder)=%v Gated(decision)=%v, want the exempt kind ungated and the other still under the model cap",
			s.Gated("p", "embedder"), s.Gated("p", "decision"))
	}
}

// A provider that sets only the models-in-flight cap, and no lane, is capped.
func TestCallScheduler_ModelCapAloneGatesAProviderWithNoLanes(t *testing.T) {
	s := NewCallScheduler(map[string]ProviderLanesConfig{"p": {MaxModelsInFlight: 1}})
	relX := schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	y := schedAsync(t, s, "p", "embedder", CallInfo{Model: "y"})
	eventually(t, "y waiting at the gate", func() bool { return s.Stats()["p"].Models.Waiting == 1 })
	mustStillWait(t, y, "a second model on a provider with only the model cap")
	relX()
	mustGrant(t, y, "the second model once the first drained")

	// Reloading the cap away lets a waiter go and forgets the provider.
	relY := schedNow(t, s, "p", "chat", CallInfo{Model: "y"})
	z := schedAsync(t, s, "p", "chat", CallInfo{Model: "z"})
	eventually(t, "z waiting at the gate", func() bool { return s.Stats()["p"].Models.Waiting == 1 })
	s.Reload(map[string]ProviderLanesConfig{"p": {}})
	mustGrant(t, z, "a waiter once the cap is reloaded away")
	relY()
	if st := s.Stats(); st != nil {
		t.Fatalf("stats = %+v, want no capped provider left", st)
	}
}

func TestCallScheduler_CallWithoutModelSkipsModelGate(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 3}, 1))
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	schedNow(t, s, "p", "chat", CallInfo{})
	if st := s.Stats()["p"]; st.Lanes["chat"].Active != 2 || len(st.Models.InFlight) != 1 {
		t.Fatalf("stats = %+v, want two calls in the lane and only x at the gate", st)
	}
}

func TestCallScheduler_CancelledLaneWaitLeavesModelGate(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 1}, 1))
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := s.Acquire(ctx, "p", "chat", CallInfo{Model: "x"}); errc <- err }()
	// The second call is through the gate (x is in flight) and queued on the lane.
	eventually(t, "second call queued on the lane", func() bool {
		st := s.Stats()["p"]
		return st.Lanes["chat"].Queued == 1 && st.Models.InFlight["x"] == 2
	})
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	if st := s.Stats()["p"]; st.Models.InFlight["x"] != 1 || st.Lanes["chat"].Queued != 0 {
		t.Fatalf("stats = %+v, want the cancelled call out of both the lane and the gate", st)
	}
}

func TestCallScheduler_CancelledGateWaitTakesNoLaneSlot(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 2}, 1))
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := s.Acquire(ctx, "p", "chat", CallInfo{Model: "y"}); errc <- err }()
	eventually(t, "y waiting at the gate", func() bool { return s.Stats()["p"].Models.Waiting == 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	if st := s.Stats()["p"]; st.Models.Waiting != 0 || st.Lanes["chat"].Active != 1 || st.Lanes["chat"].Grants != 1 {
		t.Fatalf("stats = %+v, want only the first call anywhere", st)
	}
}

func TestCallScheduler_ReleaseFreesLaneAndGateOnce(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 2}, 1))
	r1 := schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	r1()
	r1()
	st := s.Stats()["p"]
	if st.Lanes["chat"].Active != 1 || st.Models.InFlight["x"] != 1 {
		t.Fatalf("stats after a double release = %+v, want one call left in the lane and at the gate", st)
	}
}

func TestCallScheduler_DoneDropsReservationOnEveryLane(t *testing.T) {
	hold := LaneConfig{MaxConcurrent: 1, AffinityHold: time.Minute}
	s := NewCallScheduler(map[string]ProviderLanesConfig{
		"p": {Lanes: map[string]LaneConfig{"chat": hold, "embedder": hold}},
		"q": {Lanes: map[string]LaneConfig{"chat": hold}},
	})
	schedNow(t, s, "p", "chat", CallInfo{Owner: "run"})()
	schedNow(t, s, "p", "embedder", CallInfo{Owner: "run"})()
	schedNow(t, s, "q", "chat", CallInfo{Owner: "run"})()
	reserved := func() int {
		st := s.Stats()
		return st["p"].Lanes["chat"].Reserved + st["p"].Lanes["embedder"].Reserved + st["q"].Lanes["chat"].Reserved
	}
	if n := reserved(); n != 3 {
		t.Fatalf("reservations = %d, want one per lane the run called on", n)
	}
	s.Done("run")
	if n := reserved(); n != 0 {
		t.Fatalf("reservations after Done = %d, want 0", n)
	}
}

func TestCallScheduler_ReloadReconfiguresSurvivingLaneInPlace(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 1}, 0))
	schedNow(t, s, "p", "chat", CallInfo{})
	waiter := schedAsync(t, s, "p", "chat", CallInfo{})
	eventually(t, "waiter queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 1 })
	s.Reload(oneLane(LaneConfig{MaxConcurrent: 2}, 0))
	// The same lane, grown: its call in flight is still counted and its waiter
	// is admitted by the new cap.
	mustGrant(t, waiter, "the waiter after the lane grew on reload")
	if st := s.Stats()["p"].Lanes["chat"]; st.Active != 2 || st.MaxConcurrent != 2 || st.Grants != 2 {
		t.Fatalf("lane stats = %+v, want Active 2 of 2 with both grants on one lane", st)
	}
}

func TestCallScheduler_ReloadRemovedLaneDrains(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 1}, 0))
	rel := schedNow(t, s, "p", "chat", CallInfo{})
	waiter := schedAsync(t, s, "p", "chat", CallInfo{})
	eventually(t, "waiter queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 1 })
	s.Reload(nil)
	if st := s.Stats(); st != nil {
		t.Fatalf("Stats() = %v, want nil once the lane is removed", st)
	}
	// New calls are ungated; the old lane's waiter is still served by the old
	// lane's own release.
	schedNow(t, s, "p", "chat", CallInfo{})
	mustStillWait(t, waiter, "the removed lane's waiter while its call is in flight")
	rel()
	mustGrant(t, waiter, "the removed lane's waiter once its call ended")()
}

func TestCallScheduler_ReloadAddsLane(t *testing.T) {
	s := NewCallScheduler(nil)
	schedNow(t, s, "p", "chat", CallInfo{})
	s.Reload(oneLane(LaneConfig{MaxConcurrent: 1}, 0))
	schedNow(t, s, "p", "chat", CallInfo{})
	second := schedAsync(t, s, "p", "chat", CallInfo{})
	eventually(t, "second call queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 1 })
	mustStillWait(t, second, "a second call on the added one-slot lane")
}

func TestCallScheduler_ReloadKeepsModelGateState(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 4}, 1))
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	y := schedAsync(t, s, "p", "chat", CallInfo{Model: "y"})
	eventually(t, "y waiting at the gate", func() bool { return s.Stats()["p"].Models.Waiting == 1 })
	s.Reload(oneLane(LaneConfig{MaxConcurrent: 4}, 2))
	mustGrant(t, y, "the waiting model after the model cap rose on reload")
	if st := s.Stats()["p"].Models; st.InFlight["x"] != 1 || st.InFlight["y"] != 1 {
		t.Fatalf("model stats = %+v, want x and y in flight on the one gate", st)
	}
}

func TestCallScheduler_StatsReportsEveryLane(t *testing.T) {
	s := NewCallScheduler(map[string]ProviderLanesConfig{
		"p": {Lanes: map[string]LaneConfig{"chat": {MaxConcurrent: 2}, "embedder": {MaxConcurrent: 3}}, MaxModelsInFlight: 2},
	})
	schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	schedNow(t, s, "p", "embedder", CallInfo{Model: "e"})
	st := s.Stats()
	if len(st) != 1 || len(st["p"].Lanes) != 2 {
		t.Fatalf("Stats() = %+v, want one provider with two lanes", st)
	}
	if c, e := st["p"].Lanes["chat"], st["p"].Lanes["embedder"]; c.Active != 1 || c.MaxConcurrent != 2 || e.Active != 1 || e.MaxConcurrent != 3 {
		t.Fatalf("lane stats = %+v / %+v, want one call in each lane", c, e)
	}
	if m := st["p"].Models.InFlight; m["x"] != 1 || m["e"] != 1 {
		t.Fatalf("models in flight = %v, want x and e", m)
	}
}

func TestCallScheduler_StormNeverExceedsLaneOrModelCaps(t *testing.T) {
	const slots, maxModels = 3, 2
	s := NewCallScheduler(map[string]ProviderLanesConfig{
		"p": {
			Lanes: map[string]LaneConfig{
				"chat": {MaxConcurrent: slots, QuantumCalls: 2, AffinityHold: time.Millisecond, BackgroundShare: 0.25},
			},
			MaxModelsInFlight:  maxModels,
			ModelSwitchMaxWait: 500 * time.Microsecond,
		},
	})
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	computing := map[string]int{}
	peakModels := 0
	var wg sync.WaitGroup
	for r := 0; r < 24; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(r)))
			c := CallInfo{UserID: fmt.Sprintf("u%d", r%4), Background: r%5 == 0, Model: fmt.Sprintf("m%d", r%4)}
			if r%6 != 0 {
				c.Owner = fmt.Sprintf("run%d", r)
			}
			defer s.Done(c.Owner)
			for i := 0; i < 40; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				if rng.Intn(3) == 0 {
					go func(d time.Duration) { time.Sleep(d); cancel() }(time.Duration(rng.Intn(2000)) * time.Microsecond)
				}
				rel, err := s.Acquire(ctx, "p", "chat", c)
				if err != nil {
					cancel()
					continue
				}
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				mu.Lock()
				computing[c.Model]++
				if len(computing) > peakModels {
					peakModels = len(computing)
				}
				mu.Unlock()
				time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
				mu.Lock()
				if computing[c.Model]--; computing[c.Model] == 0 {
					delete(computing, c.Model)
				}
				mu.Unlock()
				inFlight.Add(-1)
				rel()
				if rng.Intn(8) == 0 {
					rel()
				}
				cancel()
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("storm did not finish (stats %+v)", s.Stats())
	}
	if p := int(peak.Load()); p > slots {
		t.Fatalf("peak calls in flight = %d, want <= %d", p, slots)
	}
	if peakModels > maxModels {
		t.Fatalf("peak distinct models computing = %d, want <= %d", peakModels, maxModels)
	}
	st := s.Stats()["p"]
	if l := st.Lanes["chat"]; l.Active != 0 || l.Queued != 0 || l.Reserved != 0 {
		t.Fatalf("lane stats after the storm = %+v, want an empty lane", l)
	}
	if len(st.Models.InFlight) != 0 || st.Models.Waiting != 0 {
		t.Fatalf("model stats after the storm = %+v, want an empty gate", st.Models)
	}
}

// A request that holds no admission slot is bounded by the queue it joins: with
// the bound reached it is refused at once, and takes nothing.
func TestCallScheduler_AcquireBoundedRefusesWhenTheLaneQueueIsFull(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 1}, 0))
	rel := schedNow(t, s, "p", "chat", CallInfo{})
	first := acquireAsync(t, schedCtx(t), func(ctx context.Context) (func(), error) {
		return s.AcquireBounded(ctx, "p", "chat", CallInfo{}, 1)
	})
	eventually(t, "one bounded waiter queued", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 1 })

	if _, err := s.AcquireBounded(context.Background(), "p", "chat", CallInfo{}, 1); !IsQueueFull(err) {
		t.Fatalf("AcquireBounded with the queue at its bound = %v, want a queue-full refusal", err)
	}
	if st := s.Stats()["p"].Lanes["chat"]; st.Queued != 1 || st.Active != 1 {
		t.Fatalf("lane stats after the refusal = %+v, want it to have taken nothing", st)
	}
	// An admitted run's call is never refused by the same queue.
	admitted := schedAsync(t, s, "p", "chat", CallInfo{Owner: "run"})
	eventually(t, "the run's call queued past the bound", func() bool { return s.Stats()["p"].Lanes["chat"].Queued == 2 })
	rel()
	mustGrant(t, first, "the bounded waiter")()
	mustGrant(t, admitted, "the run's call")()
}

// The same bound holds at the model gate, which a bounded caller reaches first.
func TestCallScheduler_AcquireBoundedRefusesWhenTheModelGateQueueIsFull(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 4}, 1))
	relX := schedNow(t, s, "p", "chat", CallInfo{Model: "x"})
	if _, err := s.AcquireBounded(context.Background(), "p", "chat", CallInfo{Model: "y"}, 0); !IsQueueFull(err) {
		t.Fatalf("AcquireBounded for a second model with bound 0 = %v, want a queue-full refusal", err)
	}
	if st := s.Stats()["p"]; st.Models.Waiting != 0 || st.Lanes["chat"].Active != 1 {
		t.Fatalf("stats after the refusal = %+v, want it to have taken nothing", st)
	}
	// A free slot is never refused, whatever the bound.
	rel, err := s.AcquireBounded(context.Background(), "p", "chat", CallInfo{Model: "x"}, 0)
	if err != nil {
		t.Fatalf("AcquireBounded with a free slot = %v, want a grant", err)
	}
	rel()
	relX()
}

// queueReports collects what a call's OnQueued saw.
type queueReports struct {
	mu  sync.Mutex
	got []CallQueueInfo
}

func (r *queueReports) on(q CallQueueInfo) {
	r.mu.Lock()
	r.got = append(r.got, q)
	r.mu.Unlock()
}

func (r *queueReports) list() []CallQueueInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CallQueueInfo(nil), r.got...)
}

// TestCallScheduler_ReportsEachWaitBeforeItBlocks — a call that has to wait is
// told why and where before it blocks, once per wait: for its model, then for a
// lane slot. A call granted at once is told nothing. The lane position follows
// the round-robin across users, not arrival order.
func TestCallScheduler_ReportsEachWaitBeforeItBlocks(t *testing.T) {
	s := NewCallScheduler(oneLane(LaneConfig{MaxConcurrent: 1}, 1))
	var first queueReports
	holdA := schedNow(t, s, "p", "chat", CallInfo{TenantID: "t", UserID: "a", Model: "m1", OnQueued: first.on})
	if got := first.list(); len(got) != 0 {
		t.Fatalf("a call granted at once reported %+v", got)
	}

	// m2 waits for its model: m1 holds the only model place.
	var model queueReports
	m2 := schedAsync(t, s, "p", "chat", CallInfo{TenantID: "t", UserID: "c", Model: "m2", OnQueued: model.on})
	eventually(t, "m2 reported", func() bool { return len(model.list()) == 1 })
	if got := model.list()[0]; got != (CallQueueInfo{Reason: CallQueueReasonModel, Position: 1}) {
		t.Fatalf("m2 reported %+v, want models_in_flight position 1", got)
	}

	// Two more m1 calls from user a, then one from user b: b is served second,
	// ahead of a3. Each place is the one it got when it queued.
	var a2, a3, b1 queueReports
	ra2 := schedAsync(t, s, "p", "chat", CallInfo{TenantID: "t", UserID: "a", Model: "m1", OnQueued: a2.on})
	eventually(t, "a2 queued", func() bool { return len(a2.list()) == 1 })
	ra3 := schedAsync(t, s, "p", "chat", CallInfo{TenantID: "t", UserID: "a", Model: "m1", OnQueued: a3.on})
	eventually(t, "a3 queued", func() bool { return len(a3.list()) == 1 })
	rb1 := schedAsync(t, s, "p", "chat", CallInfo{TenantID: "t", UserID: "b", Model: "m1", OnQueued: b1.on})
	eventually(t, "b1 queued", func() bool { return len(b1.list()) == 1 })
	for name, tc := range map[string]struct {
		r    *queueReports
		want int
	}{"a2": {&a2, 1}, "a3": {&a3, 2}, "b1": {&b1, 2}} {
		if got := tc.r.list()[0]; got != (CallQueueInfo{Reason: CallQueueReasonLane, Position: tc.want}) {
			t.Errorf("%s reported %+v, want lane position %d", name, got, tc.want)
		}
	}

	holdA()
	mustGrant(t, ra2, "a2")()
	mustGrant(t, rb1, "b1")()
	mustGrant(t, ra3, "a3")()
	// m1 has left: m2 gets its model, then the free lane slot without waiting.
	mustGrant(t, m2, "m2")()
	if got := model.list(); len(got) != 1 {
		t.Fatalf("m2 reported %+v, want only its model wait", got)
	}
}
