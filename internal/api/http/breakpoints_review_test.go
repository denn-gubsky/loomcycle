package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/breakpoints"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// walkUnderReview seeds a live walk's run with its breakpoint set armed as
// given, the way TeamDef op=run opens it.
func walkUnderReview(t *testing.T, h *reviewHarness, armed ...string) (string, *breakpoints.Set) {
	t.Helper()
	walkRunID := seedRunInTenant(t, h.st, "", "u1", "team:plan")
	set, release, err := h.srv.breakpointReg.Open(walkRunID, armed, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return walkRunID, set
}

// startWalkMember runs a member of the walk's starter state and returns its
// run id once it is held for review. exclude names members already found, so
// a second member of the same walk is told apart from the first.
func startWalkMember(t *testing.T, h *reviewHarness, walkRunID string, set *breakpoints.Set, state string, exclude ...string) (string, <-chan teamrun.SpawnResult) {
	t.Helper()
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	ctx = tools.WithRunID(ctx, walkRunID)
	ctx = teamrun.WithReviewArming(ctx, func(context.Context) bool { return set.Armed(state, breakpoints.Review) })
	done := make(chan teamrun.SpawnResult, 1)
	go func() {
		res, _ := h.srv.runTeamMember(ctx, "writer", teamrun.Prompt{Input: "write the plan"}, "")
		done <- res
	}()
	skip := map[string]bool{walkRunID: true}
	for _, id := range exclude {
		skip[id] = true
	}
	var runID string
	waitFor(t, "the member to be held", func() bool {
		runs, _ := h.st.ListActiveRunsByUser(context.Background(), "", "u1", store.RunRunning)
		for _, r := range runs {
			if !skip[r.ID] && heldForReview(context.Background(), h.st, r.ID) {
				runID = r.ID
			}
		}
		return runID != ""
	})
	return runID, done
}

func (h *reviewHarness) putBreakpoints(walkRunID, body string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPut, h.ts.URL+"/v1/runs/"+walkRunID+"/breakpoints", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("PUT breakpoints = %d", resp.StatusCode)
	}
}

// Disarming a state's review through the breakpoint set releases its held
// members now, as approved — not at their hold's next heartbeat, 30 seconds
// away. A member of a state that is still armed stays held.
func TestBreakpoints_DisarmingReviewReleasesTheStatesHeldMembersNow(t *testing.T) {
	h := newReviewHarness(t)
	walkRunID, set := walkUnderReview(t, h, "draft:review", "polish:review")
	draft, draftDone := startWalkMember(t, h, walkRunID, set, "draft")
	polish, polishDone := startWalkMember(t, h, walkRunID, set, "polish", draft)

	h.putBreakpoints(walkRunID, `{"breakpoints":["polish:review"]}`)

	select {
	case res := <-draftDone:
		if res.Status != string(store.RunCompleted) || res.RunID != draft {
			t.Errorf("released member = %+v, want %s completed", res, draft)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disarming the state's review did not release its held member")
	}
	select {
	case res := <-polishDone:
		t.Fatalf("a member of a state still armed was released: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
	if !heldForReview(context.Background(), h.st, polish) {
		t.Fatal("the still-armed member is no longer held")
	}
	if code, body := h.review(polish, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	awaitMember(t, polishDone)
}

// A member an agent_stop hook held is not released by disarming review:
// arming did not take the hold. It waits for a verdict like any other.
func TestBreakpoints_DisarmingReviewLeavesAHooksHold(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"hold","reason":"a person should read this"}`))
	}))
	defer hook.Close()
	h := newReviewHarness(t)
	if _, err := h.srv.testHooks().Register(&hooks.Hook{Owner: "ops", Name: "hold", Phase: hooks.PhaseAgentStop, CallbackURL: hook.URL}); err != nil {
		t.Fatal(err)
	}
	walkRunID, set := walkUnderReview(t, h, "draft:review")
	member, done := startWalkMember(t, h, walkRunID, set, "draft")
	if held, by := awaited.HeldBy(t.Context(), h.st, member); !held || by != "ops/hold" {
		t.Fatalf("held = %v by %q, want the hook's hold", held, by)
	}

	h.putBreakpoints(walkRunID, `{"breakpoints":[]}`)

	select {
	case res := <-done:
		t.Fatalf("disarming review released a hook's hold: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
	if code, body := h.review(member, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	if res := awaitMember(t, done); res.Status != string(store.RunCompleted) {
		t.Errorf("member result = %+v, want completed", res)
	}
}

// An agent state armed for review holds its member through the real server: a
// rejection with feedback revises, the revision is held again, and approving it
// hands the approved answer on as the state's output.
func TestBreakpoints_AgentStateReviewHoldsThenHandsOnTheApprovedAnswer(t *testing.T) {
	h := newReviewHarness(t)
	walkRunID, set := walkUnderReview(t, h, "draft:review")
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	ctx = tools.WithRunID(ctx, walkRunID)
	runner := teamrun.NewAgentRunner(h.srv.runTeamMember, teamrun.WithMemberReview(liveBreakpoints{set}, 0))
	st := teamgraph.State{ID: "draft", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "writer"}}

	type result struct {
		oc  teamrun.Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		oc, err := runner.RunHandler(ctx, st, &teamrun.Task{Input: "write the plan"})
		done <- result{oc, err}
	}()
	var member string
	waitFor(t, "the agent state's member to be held", func() bool {
		runs, _ := h.st.ListActiveRunsByUser(context.Background(), "", "u1", store.RunRunning)
		for _, r := range runs {
			if r.ID != walkRunID && heldForReview(context.Background(), h.st, r.ID) {
				member = r.ID
			}
		}
		return member != ""
	})
	select {
	case res := <-done:
		t.Fatalf("the state finished while its member was held: %+v", res)
	case <-time.After(100 * time.Millisecond):
	}

	if code, body := h.review(member, `{"decision":"reject","feedback":"cover the rollback"}`); code != http.StatusOK {
		t.Fatalf("reject = %d %s", code, body)
	}
	h.waitHeld(member, 2)
	if code, body := h.review(member, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, body)
	}
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("state failed: %v", res.err)
		}
		if !strings.Contains(res.oc.Output, "answer 2") {
			t.Errorf("state output = %q, want the approved revision", res.oc.Output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approving the held member did not finish the state")
	}
}
