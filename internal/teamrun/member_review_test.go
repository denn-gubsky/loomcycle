package teamrun

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// reviewOn arms review on one state, switchable while the walk runs.
type reviewOn struct {
	state string
	on    atomic.Bool
}

func (s *reviewOn) Armed(state string, phase BreakpointPhase) bool {
	return state == s.state && phase == Review && s.on.Load()
}

func armedReview(state string) *reviewOn {
	s := &reviewOn{state: state}
	s.on.Store(true)
	return s
}

// armingSpy records, per agent, whether the member was handed a live review
// arming that answered true, and the deadline it carried.
type armingSpy struct {
	mu    sync.Mutex
	armed map[string]bool
	ttl   map[string]time.Duration
}

func (s *armingSpy) spawn(ctx context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed == nil {
		s.armed, s.ttl = map[string]bool{}, map[string]time.Duration{}
	}
	a := ReviewArming(ctx)
	s.armed[agent] = a != nil && a(ctx)
	s.ttl[agent] = ReviewTTL(ctx)
	return SpawnResult{Output: "work of " + agent, Status: "completed"}, nil
}

// An armed agent or parallel state hands its own members the live arming and
// the walk's deadline — and never its consolidator, whose answer is the verdict
// on that work rather than work to review.
func TestMemberReview_AgentAndParallelMembersAreArmedButNotTheirConsolidator(t *testing.T) {
	for name, tc := range map[string]struct {
		st      teamgraph.State
		members []string
	}{
		"agent": {teamgraph.State{ID: "draft", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerAgent, Agent: "writer", Consolidator: "judge"}}, []string{"writer"}},
		"parallel": {teamgraph.State{ID: "draft", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerParallel, Agents: []string{"a", "b"}, Consolidator: "judge"}}, []string{"a", "b"}},
	} {
		t.Run(name, func(t *testing.T) {
			spy := &armingSpy{}
			r := varsRunner(spy.spawn)
			WithMemberReview(armedReview("draft"), 45*time.Second)(r)
			if _, err := r.RunHandler(context.Background(), tc.st, &Task{Input: "go"}); err != nil {
				t.Fatal(err)
			}
			for _, m := range tc.members {
				if !spy.armed[m] || spy.ttl[m] != 45*time.Second {
					t.Errorf("member %q armed=%v ttl=%v, want armed with the 45s deadline", m, spy.armed[m], spy.ttl[m])
				}
			}
			if _, ran := spy.armed["judge"]; !ran {
				t.Fatal("the consolidator never ran")
			}
			if spy.armed["judge"] || spy.ttl["judge"] != 0 {
				t.Error("the consolidator was handed the review arming")
			}
		})
	}
}

// An agent state nobody armed hands its member no arming — the path a walk took
// before review reached agent states.
func TestMemberReview_UnarmedAgentStateHandsNoArming(t *testing.T) {
	spy := &armingSpy{}
	r := varsRunner(spy.spawn)
	WithMemberReview(armedReview("elsewhere"), 0)(r)
	if _, err := r.RunHandler(context.Background(), agentState("writer"), &Task{Input: "go"}); err != nil {
		t.Fatal(err)
	}
	if spy.armed["writer"] {
		t.Error("a member of an unarmed state was armed")
	}
}

// Reaching `need` does not cancel a sibling while the state's review is armed:
// it may be held, and cancelling it would throw a person's review away. Without
// review it does, as before.
func TestParallelReview_NoShortCircuitWhileArmed(t *testing.T) {
	for name, arm := range map[string]bool{"armed": true, "unarmed": false} {
		t.Run(name, func(t *testing.T) {
			arming := &reviewOn{state: "fan"}
			arming.on.Store(arm)
			release := make(chan struct{})
			var started, cancelled atomic.Bool
			r := varsRunner(func(ctx context.Context, agent string, _ Prompt, _ string) (SpawnResult, error) {
				switch agent {
				case "fast", "judge":
					return SpawnResult{Output: agent, Status: "completed"}, nil
				}
				started.Store(true)
				select { // the slow member: held for its verdict, or still working
				case <-release:
					return SpawnResult{Output: "slow", Status: "completed"}, nil
				case <-ctx.Done():
					cancelled.Store(true)
					return SpawnResult{}, ctx.Err()
				}
			})
			WithMemberReview(arming, 0)(r)
			st := teamgraph.State{ID: "fan", Handler: teamgraph.Handler{Kind: teamgraph.HandlerParallel,
				Agents: []string{"fast", "slow"}, Wait: teamgraph.WaitAny, Consolidator: "judge"}}

			done := make(chan error, 1)
			go func() { _, err := r.RunHandler(context.Background(), st, &Task{Input: "go"}); done <- err }()
			if !arm {
				// Unarmed, the fast success must stop the slow member on its own:
				// the state finishes although the slow member is never released.
				// The cancel can land before the slow member's goroutine starts
				// its spawn, in which case it never runs at all; either way it
				// must not still be working.
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("with no review armed, reaching need did not stop the slow member")
				}
				if started.Load() && !cancelled.Load() {
					t.Error("with no review armed, the slow member ran on after need was reached")
				}
				return
			}
			select {
			case err := <-done:
				t.Fatalf("the state finished while a member was still out (err=%v, cancelled=%v)", err, cancelled.Load())
			case <-time.After(100 * time.Millisecond):
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if cancelled.Load() {
				t.Error("reaching need cancelled a member while review was armed")
			}
		})
	}
}

// Review is armable on the states whose members' work the walk hands on; a
// consolidator is refused with the reason, and a pause stays starter-only.
func TestCheckBreakpointTargets_ReviewReachesAgentAndParallelStates(t *testing.T) {
	def := teamgraph.Definition{States: []teamgraph.State{
		{ID: "wave", Handler: teamgraph.Handler{Kind: teamgraph.HandlerStarter}},
		{ID: "draft", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "w"}},
		{ID: "fan", Handler: teamgraph.Handler{Kind: teamgraph.HandlerParallel, Agents: []string{"a"}, Consolidator: "j"}},
		{ID: "judge", Handler: teamgraph.Handler{Kind: teamgraph.HandlerConsolidator, Agent: "j"}},
		{ID: "done", Handler: teamgraph.Handler{Kind: teamgraph.HandlerTerminal}},
	}}
	for spec, want := range map[string]string{
		"wave:review":          "",
		"draft:review":         "",
		"fan:review":           "",
		"judge:review":         "is a consolidator",
		"done:review":          "runs no member to hold for review",
		"draft":                "only a starter",
		"fan:before_dispatch":  "only a starter",
		"nope:review":          `has no state "nope"`,
		"wave:before_dispatch": "",
	} {
		err := CheckBreakpointTargets(def, "t", []string{spec})
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: refused: %v", spec, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err = %v, want a refusal containing %q", spec, err, want)
		}
	}
}

// A walk nested inside a member runs under a ctx that carries the ENCLOSING
// walk's review arming and deadline. Its own members take the nested walk's
// arming; its consolidators must take none — never the outer state's — or
// they are held for a review nobody armed, and a deadline turns the hold into
// a rejection that fails both walks.
func TestMemberReview_NestedWalkConsolidatorIsNotArmedByTheEnclosingWalk(t *testing.T) {
	for name, st := range map[string]teamgraph.State{
		"agent": {ID: "draft", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerAgent, Agent: "writer", Consolidator: "judge"}},
		"parallel": {ID: "draft", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerParallel, Agents: []string{"a", "b"}, Consolidator: "judge"}},
		"standalone": {ID: "draft", Handler: teamgraph.Handler{
			Kind: teamgraph.HandlerConsolidator, Agent: "judge"}},
	} {
		t.Run(name, func(t *testing.T) {
			outer := WithReviewArming(context.Background(), func(context.Context) bool { return true })
			outer = WithReviewTTL(outer, func() time.Duration { return 7 * time.Second })
			spy := &armingSpy{}
			r := varsRunner(spy.spawn)
			WithMemberReview(armedReview("nothing-armed-here"), 0)(r)
			if _, err := r.RunHandler(outer, st, &Task{Input: "go"}); err != nil {
				t.Fatal(err)
			}
			if _, ran := spy.armed["judge"]; !ran {
				t.Fatal("the consolidator never ran")
			}
			for agent, armed := range spy.armed {
				if armed || spy.ttl[agent] != 0 {
					t.Errorf("%q armed=%v ttl=%v, want no arming and no deadline from the enclosing walk",
						agent, armed, spy.ttl[agent])
				}
			}
		})
	}
}

// A nil arming or deadline clears one inherited from an enclosing walk rather
// than leaving it in place.
func TestWithReviewArming_NilClearsAnInheritedArming(t *testing.T) {
	ctx := WithReviewArming(context.Background(), func(context.Context) bool { return true })
	ctx = WithReviewTTL(ctx, func() time.Duration { return time.Minute })
	ctx = WithReviewTTL(WithReviewArming(ctx, nil), nil)
	if ReviewArming(ctx) != nil || ReviewTTL(ctx) != 0 {
		t.Errorf("after clearing: arming set=%v ttl=%v, want neither", ReviewArming(ctx) != nil, ReviewTTL(ctx))
	}
}
