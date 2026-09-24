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
