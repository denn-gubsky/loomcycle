package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// reviewSpy replaces the fixture's spawner with one that records what review
// arming each member was handed.
type reviewSpy struct {
	mu    sync.Mutex
	armed []bool
	ttls  []time.Duration
}

func (s *reviewSpy) spawn(ctx context.Context, _ string, _ teamrun.Prompt, _ string) (teamrun.SpawnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := teamrun.ReviewArming(ctx)
	s.armed = append(s.armed, a != nil && a(ctx))
	s.ttls = append(s.ttls, teamrun.ReviewTTL(ctx))
	return teamrun.SpawnResult{Output: "reviewed", Status: "completed"}, nil
}

// op=run review arms the starter's members for an operator's verdict, with the
// walk's deadline — and needs no human-ask machinery, because the verdict
// arrives through each member run's own review verb, not a walk pause.
func TestTeamDefTool_Run_ReviewArmsTheStartersMembers(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	tool.AskHuman = nil
	spy := &reviewSpy{}
	tool.Spawn = spy.spawn

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","review":["wave"],"review_ttl_seconds":30}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if len(spy.armed) != 2 || !spy.armed[0] || !spy.armed[1] {
		t.Errorf("members armed = %v, want both", spy.armed)
	}
	for _, ttl := range spy.ttls {
		if ttl != 30*time.Second {
			t.Errorf("member deadline = %v, want 30s", ttl)
		}
	}
	if out := decodeResult(t, res.Text); out["breakpoints_hit"] != nil {
		t.Errorf("review paused the walk: breakpoints_hit = %v", out["breakpoints_hit"])
	}
}

// liveDeadline is a live set whose review deadline an operator has changed.
type liveDeadline struct {
	teamrun.BreakpointSource
	ttl time.Duration
}

func (l liveDeadline) ReviewTTL() time.Duration { return l.ttl }

// op=run seeds its review_ttl_seconds into the live set, beside the specs, and
// the members read the deadline that set holds — so changing it there, while
// the walk runs, reaches the members still to be held.
func TestTeamDefTool_Run_ReviewDeadlineIsReadFromTheLiveSet(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	tool.AskHuman = nil
	spy := &reviewSpy{}
	tool.Spawn = spy.spawn
	var seeded time.Duration
	tool.LiveBreakpoints = func(_ context.Context, seed []string, reviewTTL time.Duration, _ func(string) error) (teamrun.BreakpointSource, func(), error) {
		seeded = reviewTTL
		src, err := teamrun.NewStaticBreakpoints(seed)
		return liveDeadline{src, 5 * time.Minute}, func() {}, err
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","review":["wave"],"review_ttl_seconds":30}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if seeded != 30*time.Second {
		t.Errorf("live set seeded with %v, want the run's 30s", seeded)
	}
	if len(spy.ttls) != 2 {
		t.Fatalf("members = %d, want 2", len(spy.ttls))
	}
	for _, ttl := range spy.ttls {
		if ttl != 5*time.Minute {
			t.Errorf("member deadline = %v, want the live set's 5m", ttl)
		}
	}
}

// The breakpoint form arms review the same way, also without a human-ask.
func TestTeamDefTool_Run_ReviewPhaseBreakpointArmsReview(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	tool.AskHuman = nil
	spy := &reviewSpy{}
	tool.Spawn = spy.spawn
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","breakpoints":["wave:review"]}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if len(spy.armed) != 2 || !spy.armed[0] {
		t.Errorf("members armed = %v, want both", spy.armed)
	}
}

// A walk run without review never arms a member.
func TestTeamDefTool_Run_NoReviewNoArming(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	spy := &reviewSpy{}
	tool.Spawn = spy.spawn
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	for i, a := range spy.armed {
		if a {
			t.Errorf("member %d armed with no review asked for", i)
		}
	}
}

// Refused at the run boundary rather than arming nothing: an unknown state, a
// state that dispatches no wave, and a debug pause with nobody to ask.
func TestTeamDefTool_Run_ReviewRefusals(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	tool.AskHuman = nil
	for name, tc := range map[string]struct{ args, want string }{
		"unknown state":             {`"review":["nope"]`, `has no state "nope"`},
		"debug pause with no human": {`"breakpoints":["wave"]`, "require the Interruption machinery"},
		"bad phase":                 {`"breakpoints":["wave:later"]`, "expected"},
	} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x",`+tc.args+`}`))
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s: isError=%v %q, want a refusal naming %q", name, res.IsError, res.Text, tc.want)
		}
	}
}

// pipelineGraph is an agent state, a parallel fan-out with its consolidator,
// and a standalone consolidator — every kind review now reaches, and the one
// it does not.
const pipelineGraph = `{"entry":"draft","states":[` +
	`{"state":"draft","handler":{"kind":"agent","agent":"writer"}},` +
	`{"state":"fan","handler":{"kind":"parallel","agents":["a","b"],"consolidator":"merge"}},` +
	`{"state":"judge","handler":{"kind":"consolidator","agent":"judge"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"draft","to":"fan","on":"success"},{"from":"fan","to":"judge","on":"success"},` +
	`{"from":"judge","to":"done","on":"success"}]}`

// agentArmingSpy records, per agent, whether its member was armed for review.
type agentArmingSpy struct {
	mu    sync.Mutex
	armed map[string]bool
}

func (s *agentArmingSpy) spawn(ctx context.Context, agent string, _ teamrun.Prompt, _ string) (teamrun.SpawnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := teamrun.ReviewArming(ctx)
	s.armed[agent] = a != nil && a(ctx)
	return teamrun.SpawnResult{Output: "work of " + agent, Status: "completed"}, nil
}

// op=run review arms an agent state's member and each member of a parallel
// fan-out — never the consolidator that merges or judges them.
func TestTeamDefTool_Run_ReviewArmsAgentAndParallelMembers(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	spy := &agentArmingSpy{armed: map[string]bool{}}
	tool.Spawn = spy.spawn
	createTeam(t, tool, ctx, "pipeline", pipelineGraph)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"pipeline","input":"x","review":["draft","fan"]}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	for _, m := range []string{"writer", "a", "b"} {
		if !spy.armed[m] {
			t.Errorf("member %q was not armed for review: %v", m, spy.armed)
		}
	}
	for _, c := range []string{"merge", "judge"} {
		if armed, ran := spy.armed[c]; !ran || armed {
			t.Errorf("consolidator %q ran=%v armed=%v, want it run and unarmed", c, ran, armed)
		}
	}
}

// Review on a consolidator is refused at the run boundary, in either spelling,
// with the reason — its answer is the verdict on the work, not the work.
func TestTeamDefTool_Run_ReviewOnAConsolidatorIsRefused(t *testing.T) {
	tool, ctx, done := teamDefFixture(t)
	defer done()
	spy := &agentArmingSpy{armed: map[string]bool{}}
	tool.Spawn = spy.spawn
	createTeam(t, tool, ctx, "pipeline", pipelineGraph)

	for _, args := range []string{`"review":["judge"]`, `"breakpoints":["judge:review"]`} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"pipeline","input":"x",`+args+`}`))
		if !res.IsError || !strings.Contains(res.Text, "is a consolidator") {
			t.Errorf("%s: isError=%v %q, want a refusal naming the consolidator", args, res.IsError, res.Text)
		}
	}
	if len(spy.armed) != 0 {
		t.Errorf("a refused run spawned %v", spy.armed)
	}
}
