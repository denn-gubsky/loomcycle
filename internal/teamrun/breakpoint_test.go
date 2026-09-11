package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// threeMessages is the fixture every breakpoint test walks: a wave of three, so
// "release one, look, release the rest" has something to distinguish it from
// both "release nothing" and "release everything".
func threeMessages() *fakeChannels {
	return &fakeChannels{inbox: []ChannelMessage{
		{ID: "m1", Payload: json.RawMessage(`{"pr":1}`)},
		{ID: "m2", Payload: json.RawMessage(`{"pr":2}`)},
		{ID: "m3", Payload: json.RawMessage(`{"pr":3}`)},
	}}
}

// countingSpawn records the order runs were spawned in, safely under fan-out.
type countingSpawn struct {
	mu      sync.Mutex
	prompts []string
}

// fn HONOURS ctx, because that is what a real spawn does — and because a fake
// that ignores it cannot tell a run that was cancelled from one that ran. The
// short-circuit test below is only meaningful against a spawner that notices.
func (c *countingSpawn) fn(ctx context.Context, _ string, p Prompt, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := p.DataSlots[StarterMessageSlot]
	c.prompts = append(c.prompts, msg)
	return "verdict for " + msg, nil
}

func (c *countingSpawn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prompts)
}

func breakRunner(t *testing.T, ch *fakeChannels, spawn SpawnFunc, states []string, f BreakpointFunc) *agentRunner {
	t.Helper()
	r := starterRunner(ch, spawn)
	WithBreakpoints(states, f)(r)
	return r
}

// TestBreakpoint_BeforeDispatchHoldsEveryRunUntilReleased: the pause happens
// with the wave composed and NOTHING spawned. This is the property the whole
// phase exists for — an operator who aborts here has run no agent and spent no
// tokens.
func TestBreakpoint_BeforeDispatchHoldsEveryRunUntilReleased(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	var spawnedAtAsk int
	r := breakRunner(t, ch, spawn.fn, []string{"wave:before_dispatch"},
		func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
			spawnedAtAsk = spawn.count()
			if bp.Phase != BeforeDispatch {
				t.Errorf("phase = %q, want before_dispatch", bp.Phase)
			}
			return BreakDecision{Action: BreakContinue}, nil
		})

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if spawnedAtAsk != 0 {
		t.Errorf("%d runs had already been spawned when the breakpoint asked — before_dispatch must hold every run", spawnedAtAsk)
	}
	if got := spawn.count(); got != 3 {
		t.Errorf("spawned %d runs after continue, want 3", got)
	}
}

// TestBreakpoint_BeforeDispatchReleasesExactlyN: a staged release dispatches
// only the runs the operator released, and pauses again with the rest.
func TestBreakpoint_BeforeDispatchReleasesExactlyN(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	var pendings, spawnedBefore []int
	r := breakRunner(t, ch, spawn.fn, []string{"wave:before_dispatch"},
		func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
			pendings = append(pendings, bp.Pending)
			spawnedBefore = append(spawnedBefore, spawn.count())
			if len(pendings) == 1 {
				return BreakDecision{Action: BreakRelease, N: 1}, nil
			}
			return BreakDecision{Action: BreakContinue}, nil
		})

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if want := []int{3, 2}; !equalInts(pendings, want) {
		t.Errorf("pending at each pause = %v, want %v", pendings, want)
	}
	// The count at the SECOND ask is the proof the release was bounded: exactly
	// the one released ran, not the whole wave.
	if want := []int{0, 1}; !equalInts(spawnedBefore, want) {
		t.Errorf("runs spawned at each pause = %v, want %v", spawnedBefore, want)
	}
	if got := spawn.count(); got != 3 {
		t.Errorf("spawned %d in total, want 3", got)
	}
}

// TestBreakpoint_BeforeDispatchAbortSpawnsNothing: the zero-value decision is
// abort, so a caller that answers with an empty struct fails safe.
func TestBreakpoint_BeforeDispatchAbortSpawnsNothing(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	r := breakRunner(t, ch, spawn.fn, []string{"wave"},
		func(context.Context, Breakpoint) (BreakDecision, error) {
			return BreakDecision{}, nil // zero value
		})

	_, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"})
	if err == nil {
		t.Fatal("an aborted breakpoint must fail the walk")
	}
	if got := spawn.count(); got != 0 {
		t.Errorf("spawned %d runs after an abort, want 0", got)
	}
	if n := len(ch.sinks(t)); n != 0 {
		t.Errorf("published %d sink messages after an abort, want 0", n)
	}
	// Ack is gated on the wave succeeding, so an aborted wave redelivers.
	if len(ch.acked) != 0 {
		t.Errorf("acked %v after an abort — the batch must redeliver", ch.acked)
	}
}

// TestBreakpoint_PromptPreviewCarriesTheComposedMessage: the pause shows what
// each agent WILL be asked, which is the thing no channel inspection could ever
// show — the composed prompt never touches a channel.
func TestBreakpoint_PromptPreviewCarriesTheComposedMessage(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	var seen []PromptPreview
	r := breakRunner(t, ch, spawn.fn, []string{"wave:before_dispatch"},
		func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
			seen = bp.Prompts
			return BreakDecision{Action: BreakContinue}, nil
		})
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("previewed %d prompts, want 3", len(seen))
	}
	for i, p := range seen {
		if p.Index != i {
			t.Errorf("preview[%d].Index = %d", i, p.Index)
		}
		if p.Agent != "reviewer" {
			t.Errorf("preview[%d].Agent = %q, want reviewer", i, p.Agent)
		}
		if want := fmt.Sprintf(`{"pr":%d}`, i+1); p.Message != want {
			t.Errorf("preview[%d].Message = %q, want %q", i, p.Message, want)
		}
		if p.System != "You review." || !strings.Contains(p.Input, StarterMessageSlot) {
			t.Errorf("preview[%d] lost the node's templates: %+v", i, p)
		}
	}
}

// TestBreakpoint_AfterCollectionWithholdsEverySinkMessage: every run is DONE and
// the sink is still empty. That is the whole claim of the phase — the next stage
// must not see a result the operator has not released.
func TestBreakpoint_AfterCollectionWithholdsEverySinkMessage(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	var ranAtAsk, publishedAtAsk int
	var results []BreakpointResult
	r := breakRunner(t, ch, spawn.fn, []string{"wave:after_collection"},
		func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
			ranAtAsk, publishedAtAsk = spawn.count(), len(ch.sinks(t))
			results = bp.Results
			return BreakDecision{Action: BreakContinue}, nil
		})

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if ranAtAsk != 3 {
		t.Errorf("%d runs had finished when after_collection asked, want 3", ranAtAsk)
	}
	if publishedAtAsk != 0 {
		t.Errorf("%d sink messages were already published when after_collection asked — the phase must withhold all of them", publishedAtAsk)
	}
	if len(results) != 3 || results[0].Output != `verdict for {"pr":1}` {
		t.Errorf("result preview = %+v, want the three outputs", results)
	}
	if n := len(ch.sinks(t)); n != 3 {
		t.Errorf("published %d sink messages after continue, want 3", n)
	}
}

// TestBreakpoint_AfterCollectionReleasesExactlyN: a staged publish moves exactly
// the released results onto the sink, in wave order.
func TestBreakpoint_AfterCollectionReleasesExactlyN(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	var publishedAtAsk []int
	r := breakRunner(t, ch, spawn.fn, []string{"wave:after_collection"},
		func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
			publishedAtAsk = append(publishedAtAsk, len(ch.sinks(t)))
			if len(publishedAtAsk) == 1 {
				return BreakDecision{Action: BreakRelease, N: 2}, nil
			}
			return BreakDecision{Action: BreakContinue}, nil
		})

	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if want := []int{0, 2}; !equalInts(publishedAtAsk, want) {
		t.Errorf("sink depth at each pause = %v, want %v", publishedAtAsk, want)
	}
	sinks := ch.sinks(t)
	if len(sinks) != 3 {
		t.Fatalf("published %d, want 3", len(sinks))
	}
	for i, m := range sinks {
		if m.Index != i {
			t.Errorf("sink[%d].Index = %d — a staged release must publish in wave order", i, m.Index)
		}
		if m.WaveSize != 3 {
			t.Errorf("sink[%d].WaveSize = %d, want 3 — the width must not shrink to the released count", i, m.WaveSize)
		}
	}
}

// TestBreakpoint_AfterCollectionAbortWithholdsTheUnreleased: rejecting a wave
// means the next stage never sees it. A flush-on-abort would make the one thing
// this breakpoint exists to do untrue.
func TestBreakpoint_AfterCollectionAbortWithholdsTheUnreleased(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	asks := 0
	r := breakRunner(t, ch, spawn.fn, []string{"wave:after_collection"},
		func(context.Context, Breakpoint) (BreakDecision, error) {
			asks++
			if asks == 1 {
				return BreakDecision{Action: BreakRelease, N: 1}, nil
			}
			return BreakDecision{Action: BreakAbort}, nil
		})

	_, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"})
	if err == nil {
		t.Fatal("an aborted breakpoint must fail the walk")
	}
	// The one the operator released stays published; the two they rejected do
	// not appear.
	if n := len(ch.sinks(t)); n != 1 {
		t.Errorf("published %d sink messages, want 1 — only the released result may reach the sink", n)
	}
	if spawn.count() != 3 {
		t.Errorf("the runs themselves still happened: spawned %d, want 3", spawn.count())
	}
}

// TestBreakpoint_PhaseScopedArmingPausesOnlyThatPhase: "state:after_collection"
// must not stop the walk before dispatch, or the phase suffix means nothing.
func TestBreakpoint_PhaseScopedArmingPausesOnlyThatPhase(t *testing.T) {
	for _, tc := range []struct {
		arg   string
		phase BreakpointPhase
		want  []BreakpointPhase
	}{
		{"wave:before_dispatch", BeforeDispatch, []BreakpointPhase{BeforeDispatch}},
		{"wave:after_collection", AfterCollection, []BreakpointPhase{AfterCollection}},
		{"wave", "", []BreakpointPhase{BeforeDispatch, AfterCollection}},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			ch := threeMessages()
			spawn := &countingSpawn{}
			var phases []BreakpointPhase
			r := breakRunner(t, ch, spawn.fn, []string{tc.arg},
				func(_ context.Context, bp Breakpoint) (BreakDecision, error) {
					phases = append(phases, bp.Phase)
					return BreakDecision{Action: BreakContinue}, nil
				})
			if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
				t.Fatalf("starter: %v", err)
			}
			if len(phases) != len(tc.want) {
				t.Fatalf("paused at %v, want %v", phases, tc.want)
			}
			for i := range phases {
				if phases[i] != tc.want[i] {
					t.Errorf("pause[%d] = %q, want %q", i, phases[i], tc.want[i])
				}
			}
		})
	}
}

// TestBreakpoint_UnarmedStateTakesTheOriginalPath: a walk that arms a different
// state never asks, and publishes as it goes — the no-breakpoints behaviour.
func TestBreakpoint_UnarmedStateTakesTheOriginalPath(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	r := breakRunner(t, ch, spawn.fn, []string{"some-other-state"},
		func(context.Context, Breakpoint) (BreakDecision, error) {
			t.Error("an unarmed state must never pause")
			return BreakDecision{Action: BreakAbort}, nil
		})
	if _, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if n := len(ch.sinks(t)); n != 3 {
		t.Errorf("published %d, want 3", n)
	}
}

// TestBreakpoint_AskErrorAbortsTheWalk: a debugger whose operator cannot be
// reached must not release the wave.
func TestBreakpoint_AskErrorAbortsTheWalk(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	sentinel := errors.New("interruption timed out")
	r := breakRunner(t, ch, spawn.fn, []string{"wave:before_dispatch"},
		func(context.Context, Breakpoint) (BreakDecision, error) {
			return BreakDecision{Action: BreakContinue}, sentinel
		})
	_, err := r.RunHandler(context.Background(), starterState(), &Task{Input: "go", WalkID: "wlk_t"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the ask's error", err)
	}
	if spawn.count() != 0 {
		t.Errorf("spawned %d runs despite an unanswerable breakpoint, want 0", spawn.count())
	}
}

// TestBreakpoint_StagedDispatchDoesNotShortCircuit: the wait threshold stops
// runs nobody is waiting for, but an operator stepping through a wave released
// each batch DELIBERATELY. Cancelling one because an earlier batch already met
// the threshold would make the debugger lie about what it ran.
func TestBreakpoint_StagedDispatchDoesNotShortCircuit(t *testing.T) {
	ch := threeMessages()
	spawn := &countingSpawn{}
	st := starterState()
	st.Handler.Fanout.Wait = teamgraph.WaitAtLeast + ":1"

	asks := 0
	r := breakRunner(t, ch, spawn.fn, []string{"wave:before_dispatch"},
		func(context.Context, Breakpoint) (BreakDecision, error) {
			asks++
			return BreakDecision{Action: BreakRelease, N: 1}, nil
		})
	if _, err := r.RunHandler(context.Background(), st, &Task{Input: "go", WalkID: "wlk_t"}); err != nil {
		t.Fatalf("starter: %v", err)
	}
	if asks != 3 {
		t.Errorf("asked %d times, want 3 — every step must be offered", asks)
	}
	if got := spawn.count(); got != 3 {
		t.Errorf("spawned %d runs, want 3 — a released run must actually run even once at_least is met", got)
	}
	for i, m := range ch.sinks(t) {
		if m.Status != SinkOK {
			t.Errorf("sink[%d] = %q (%s), want ok — a released run must not be cancelled", i, m.Status, m.Error)
		}
	}
}

func TestParseBreakpoint(t *testing.T) {
	for _, tc := range []struct {
		in    string
		id    string
		phase BreakpointPhase
		ok    bool
	}{
		{"wave", "wave", "", true},
		{"wave:before_dispatch", "wave", BeforeDispatch, true},
		{"wave:after_collection", "wave", AfterCollection, true},
		{"wave:typo", "", "", false},
		{"", "", "", false},
		{":before_dispatch", "", "", false},
		// A state id may itself contain a colon, so only the LAST segment is a
		// phase candidate — and if it is not a phase, the whole thing is refused
		// rather than silently read as an id.
		{"team:wave:after_collection", "team:wave", AfterCollection, true},
	} {
		id, phase, ok := ParseBreakpoint(tc.in)
		if id != tc.id || phase != tc.phase || ok != tc.ok {
			t.Errorf("ParseBreakpoint(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, id, phase, ok, tc.id, tc.phase, tc.ok)
		}
	}
	if err := ValidateBreakpoints([]string{"wave", "wave:typo"}); err == nil {
		t.Error("ValidateBreakpoints must refuse an unknown phase")
	}
	if err := ValidateBreakpoints([]string{"wave", "wave:after_collection"}); err != nil {
		t.Errorf("ValidateBreakpoints refused a valid pair: %v", err)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
