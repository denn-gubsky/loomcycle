package teamrun

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// mutableSource is a BreakpointSource an operator can change while the walk is
// running — the in-test stand-in for the run-scoped registry.
type mutableSource struct {
	mu sync.RWMutex
	at map[string]map[BreakpointPhase]bool
}

func newMutableSource() *mutableSource {
	return &mutableSource{at: map[string]map[BreakpointPhase]bool{}}
}

func (m *mutableSource) Armed(state string, phase BreakpointPhase) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.at[state][phase]
}

func (m *mutableSource) arm(state string, phases ...BreakpointPhase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.at[state] == nil {
		m.at[state] = map[BreakpointPhase]bool{}
	}
	for _, p := range phases {
		m.at[state][p] = true
	}
}

// TestBreakpoint_ArmingMidWalkPausesTheNextWave is the requirement the
// dispatch-time argument could not meet: you start a run expecting it to work,
// watch a wave go wrong, and stop before the next one. Nothing can be passed at
// that moment, so the walk has to re-read its arming.
func TestBreakpoint_ArmingMidWalkPausesTheNextWave(t *testing.T) {
	src := newMutableSource()
	spawn := &countingSpawn{}
	var pauses []BreakpointPhase
	ask := func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
		pauses = append(pauses, bp.Phase)
		return BreakDecision{Action: BreakContinue}, nil
	}

	// Wave 1: nobody has armed anything. It must run straight through.
	ch := threeMessages()
	r := starterRunner(ch, spawn.fn)
	WithBreakpoints(src, ask)(r)
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("wave 1: %v", err)
	}
	if len(pauses) != 0 {
		t.Fatalf("wave 1 paused %v with nothing armed", pauses)
	}
	if got := len(ch.sinks(t)); got != 3 {
		t.Fatalf("wave 1 published %d sink messages, want 3", got)
	}

	// The operator watches that wave, does not like it, and hits Debug.
	src.arm("wave", BeforeDispatch)

	// Wave 2 — the SAME runner, already constructed — must now pause.
	ch2 := threeMessages()
	r2 := starterRunner(ch2, spawn.fn)
	WithBreakpoints(src, ask)(r2)
	if _, err := r2.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("wave 2: %v", err)
	}
	if len(pauses) != 1 || pauses[0] != BeforeDispatch {
		t.Errorf("wave 2 paused %v, want one before_dispatch — arming after dispatch had no effect", pauses)
	}
}

// TestBreakpoint_ArmingMidWaveHoldsWhatHasNotPublished: arming while a wave is
// in flight holds the results that have not reached the sink yet. What already
// went out stays out — the next stage has seen it, and a debugger that claimed
// otherwise would be lying.
func TestBreakpoint_ArmingMidWaveHoldsWhatHasNotPublished(t *testing.T) {
	src := newMutableSource()
	ch := threeMessages()

	firstDone := make(chan struct{}) // run 0 has returned
	release := make(chan struct{})   // runs 1+2 may proceed
	var once sync.Once
	spawn := func(ctx context.Context, _ string, p Prompt, _ string) (string, error) {
		if p.DataSlots[StarterMessageSlot] == `{"pr":1}` {
			once.Do(func() { close(firstDone) })
			return "first", nil
		}
		<-release
		return "later", nil
	}

	var seen []BreakpointResult
	r := starterRunner(ch, spawn)
	WithBreakpoints(src, func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
		seen = bp.Results
		return BreakDecision{Action: BreakContinue}, nil
	})(r)

	go func() {
		<-firstDone
		// Run 0's sink message is already out; arm, then let the rest finish.
		for len(ch.sinks(t)) == 0 {
		}
		src.arm("wave", AfterCollection)
		close(release)
	}()

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	// The pause saw only what was still held — never the one already published.
	if len(seen) != 2 {
		t.Fatalf("the pause offered %d results, want the 2 that had not published: %+v", len(seen), seen)
	}
	for _, res := range seen {
		if res.Index == 0 {
			t.Errorf("the pause offered a result that had already reached the sink: %+v", res)
		}
	}
	// And every run still produced exactly one sink message — the count is the
	// contract, whatever the arming did.
	if got := len(ch.sinks(t)); got != 3 {
		t.Errorf("published %d sink messages, want 3", got)
	}
}

// TestBreakpoint_DisarmingMidWalkStopsPausing: Debug goes off as well as on.
func TestBreakpoint_DisarmingMidWalkStopsPausing(t *testing.T) {
	src := newMutableSource()
	src.arm("wave", BeforeDispatch)
	spawn := &countingSpawn{}
	asks := 0

	ch := threeMessages()
	r := starterRunner(ch, spawn.fn)
	WithBreakpoints(src, func(context.Context, Breakpoint) (BreakDecision, error) {
		asks++
		// Turn Debug off from inside the pause, the way an operator would.
		src.mu.Lock()
		src.at = map[string]map[BreakpointPhase]bool{}
		src.mu.Unlock()
		return BreakDecision{Action: BreakRelease, N: 1}, nil
	})(r)
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	// It asked once; with the state disarmed, the remaining two dispatched
	// without another pause.
	if asks != 1 {
		t.Errorf("asked %d times, want 1 — disarming mid-pause must stop the staging", asks)
	}
	if spawn.count() != 3 {
		t.Errorf("spawned %d, want 3 — disarming must release the rest, not strand them", spawn.count())
	}
}

// TestStage_RefusesToSpinWhenNothingRetires: a release that does not shrink the
// pending set would ask a human the same question forever. A wedged walk is far
// harder to diagnose than an error naming the bug.
func TestStage_RefusesToSpinWhenNothingRetires(t *testing.T) {
	pending := func() []int { return []int{0, 1} } // never shrinks
	err := stage(context.Background(), pending,
		func([]int) (BreakDecision, error) { return BreakDecision{Action: BreakRelease, N: 1}, nil },
		func([]int) error { return nil })
	if err == nil {
		t.Fatal("stage looped on a pending set that never shrank instead of failing")
	}
}

// TestStage_ReleasesNonContiguousIndices: the pending set is not always a
// suffix — a mid-wave arm holds whichever runs happened to still be going.
func TestStage_ReleasesNonContiguousIndices(t *testing.T) {
	done := map[int]bool{}
	pending := func() []int {
		var out []int
		for _, i := range []int{1, 4, 7} {
			if !done[i] {
				out = append(out, i)
			}
		}
		return out
	}
	var released []int
	err := stage(context.Background(), pending,
		func([]int) (BreakDecision, error) { return BreakDecision{Action: BreakRelease, N: 2}, nil },
		func(idx []int) error {
			released = append(released, idx...)
			for _, i := range idx {
				done[i] = true
			}
			return nil
		})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if len(released) != 3 || released[0] != 1 || released[1] != 4 || released[2] != 7 {
		t.Errorf("released %v, want [1 4 7] in order", released)
	}
}

var _ = json.Marshal
