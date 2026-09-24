package http

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A team member reports its terminal status, so a walk can tell a rejected
// member from a failed one — which the error alone cannot, since a rejected
// run did its work and returns no error.
func TestTeamMember_ReportsItsTerminalStatus(t *testing.T) {
	h := newReviewHarness(t)
	res, err := h.srv.runTeamMember(context.Background(), "writer", teamrun.Prompt{Input: "go"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != string(store.RunCompleted) || res.RunID == "" {
		t.Errorf("result = %+v, want completed with a run id", res)
	}
	if run, _ := h.st.GetRun(context.Background(), res.RunID); string(run.Status) != res.Status {
		t.Errorf("reported %q, the row says %q", res.Status, run.Status)
	}
}

// memberUnderReview starts a team member the walk arms for review and returns
// its run id once it is held.
func memberUnderReview(t *testing.T, h *reviewHarness) (string, <-chan teamrun.SpawnResult) {
	t.Helper()
	// A walk's members run under the walk's identity; the user is what lets
	// the test find the run.
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	ctx = teamrun.WithReviewArming(ctx, func(context.Context) bool { return true })
	done := make(chan teamrun.SpawnResult, 1)
	go func() {
		res, _ := h.srv.runTeamMember(ctx, "writer", teamrun.Prompt{Input: "write the plan"}, "")
		done <- res
	}()
	var runID string
	deadline := time.Now().Add(3 * time.Second)
	for runID == "" && time.Now().Before(deadline) {
		runs, _ := h.st.ListActiveRunsByUser(context.Background(), "u1", store.RunRunning)
		for _, r := range runs {
			if heldForReview(context.Background(), h.st, r.ID) {
				runID = r.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("the armed member was never held")
	}
	return runID, done
}

func awaitMember(t *testing.T, done <-chan teamrun.SpawnResult) teamrun.SpawnResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("the member never returned")
	}
	return teamrun.SpawnResult{}
}

// A member the walk arms is held when it finishes, is REACHABLE by the
// ordinary review verb (it has a steer queue now), and a rejection ends it
// rejected — reported as such to the walk.
func TestTeamMember_ArmedMemberIsHeldAndReviewable(t *testing.T) {
	h := newReviewHarness(t)
	runID, done := memberUnderReview(t, h)
	select {
	case <-done:
		t.Fatal("a held member returned to the walk without a verdict")
	case <-time.After(50 * time.Millisecond):
	}
	if code, body := h.review(runID, `{"decision":"reject"}`); code != http.StatusOK {
		t.Fatalf("verdict on a member = %d %s — the member is not reachable", code, body)
	}
	if res := awaitMember(t, done); res.Status != string(store.RunRejected) || res.RunID != runID {
		t.Errorf("member result = %+v, want rejected", res)
	}
}

// Feedback reaches the member and it revises; approving the revision completes it.
func TestTeamMember_RevisesOnFeedbackThenCompletes(t *testing.T) {
	h := newReviewHarness(t)
	runID, done := memberUnderReview(t, h)
	if code, _ := h.review(runID, `{"decision":"reject","feedback":"cover the rollback"}`); code != http.StatusOK {
		t.Fatalf("reject = %d", code)
	}
	h.waitHeld(runID, 2)
	if seen := h.prov.seen(); len(seen) != 2 || seen[1] != "cover the rollback" {
		t.Errorf("model saw %q, want the feedback as the revision's turn", seen)
	}
	if code, _ := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d", code)
	}
	if res := awaitMember(t, done); res.Status != string(store.RunCompleted) {
		t.Errorf("member result = %+v, want completed", res)
	}
}

// The status rule and the terminal write cannot drift: for each way a run can
// end, the row finishRunWithCancel writes is the status terminalStatusOf gives.
func TestTerminalStatusOf_MatchesTheWrittenRow(t *testing.T) {
	apiCancelled, cancelAPI := context.WithCancelCause(context.Background())
	cancelAPI(cancel.ErrCancelledByAPI)
	gone, leave := context.WithCancel(context.Background())
	leave()
	for name, tc := range map[string]struct {
		ctx context.Context
		res loop.RunResult
		err error
	}{
		"completed":      {context.Background(), loop.RunResult{StopReason: "end_turn"}, nil},
		"failed":         {context.Background(), loop.RunResult{}, errors.New("boom")},
		"rejected":       {context.Background(), loop.RunResult{StopReason: loop.StopReasonRejected}, nil},
		"review expired": {context.Background(), loop.RunResult{StopReason: loop.StopReasonReviewExpired}, nil},
		"api cancel":     {apiCancelled, loop.RunResult{StopReason: "cancelled"}, context.Canceled},
		"caller left":    {gone, loop.RunResult{StopReason: "cancelled"}, context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			h := newReviewHarness(t)
			runID := seedRunInTenant(t, h.st, "", "u", "a_"+name[:3])
			h.srv.finishRunWithCancel(context.Background(), tc.ctx, runID, tc.res, tc.err, runStateMeta{})
			run, _ := h.st.GetRun(context.Background(), runID)
			if want := terminalStatusOf(tc.ctx, tc.res, tc.err); run.Status != want {
				t.Errorf("row = %q, terminalStatusOf = %q", run.Status, want)
			}
		})
	}
}

// The walk's review deadline reaches its member: a hold nobody rules on ends
// the member rejected, and the walk is told so.
func TestTeamMember_WalkDeadlineExpiresTheHold(t *testing.T) {
	h := newReviewHarness(t)
	ctx := teamrun.WithReviewArming(context.Background(), func(context.Context) bool { return true })
	ctx = teamrun.WithReviewTTL(ctx, 150*time.Millisecond)
	res, err := h.srv.runTeamMember(ctx, "writer", teamrun.Prompt{Input: "write the plan"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != teamrun.MemberRejected {
		t.Errorf("status = %q, want rejected on the deadline", res.Status)
	}
	if run, _ := h.st.GetRun(context.Background(), res.RunID); run.StopReason != "review_expired" {
		t.Errorf("stop reason = %q, want review_expired", run.StopReason)
	}
}

// teamrun cannot import the store, so it names the rejected status itself;
// the two spellings must be the same value or a rejected member reads as a
// success to the walk.
func TestTeamMember_RejectedStatusMatchesTheStore(t *testing.T) {
	if teamrun.MemberRejected != string(store.RunRejected) {
		t.Errorf("teamrun.MemberRejected = %q, store.RunRejected = %q", teamrun.MemberRejected, store.RunRejected)
	}
}

// Aborting the walk closes a member it is holding: the walk's cancellation
// reaches the member through its context, and the member ends cancelled — not
// completed on an answer nobody approved, and not left waiting for a verdict
// nobody will give.
func TestTeamMember_WalkAbortClosesAHeldMember(t *testing.T) {
	h := newReviewHarness(t)
	walkCtx, abort := context.WithCancel(tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"}))
	ctx := teamrun.WithReviewArming(walkCtx, func(context.Context) bool { return true })
	done := make(chan teamrun.SpawnResult, 1)
	go func() {
		res, _ := h.srv.runTeamMember(ctx, "writer", teamrun.Prompt{Input: "write the plan"}, "")
		done <- res
	}()
	deadline := time.Now().Add(3 * time.Second)
	held := false
	for !held && time.Now().Before(deadline) {
		runs, _ := h.st.ListActiveRunsByUser(context.Background(), "u1", store.RunRunning)
		for _, r := range runs {
			held = held || heldForReview(context.Background(), h.st, r.ID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !held {
		t.Fatal("the member was never held")
	}
	abort()
	res := awaitMember(t, done)
	if res.Status != string(store.RunCancelled) {
		t.Errorf("member after the walk aborted = %q, want cancelled", res.Status)
	}
}

// A member the walk arms records the arming and the walk's deadline in its own
// run record, which is all a restored member has to go on: its walk is gone.
func TestTeamMember_RecordsItsReviewArmingForAResume(t *testing.T) {
	h := newReviewHarness(t)
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{UserID: "u1"})
	ctx = teamrun.WithReviewArming(ctx, func(context.Context) bool { return true })
	ctx = teamrun.WithReviewTTL(ctx, time.Hour)
	done := make(chan teamrun.SpawnResult, 1)
	go func() {
		res, _ := h.srv.runTeamMember(ctx, "writer", teamrun.Prompt{Input: "write the plan"}, "")
		done <- res
	}()
	var runID string
	waitFor(t, "the member to be held", func() bool {
		runs, _ := h.st.ListActiveRunsByUser(context.Background(), "u1", store.RunRunning)
		for _, r := range runs {
			if heldForReview(context.Background(), h.st, r.ID) {
				runID = r.ID
			}
		}
		return runID != ""
	})
	run, _ := h.st.GetRun(context.Background(), runID)
	rec, _ := decodeRunConfig(run.RunConfig)
	if rec.Review == nil || !*rec.Review || rec.ReviewTTLSeconds != 3600 {
		t.Errorf("record review = %v ttl = %d, want armed with the walk's 3600s deadline", rec.Review, rec.ReviewTTLSeconds)
	}
	if code, _ := h.review(runID, `{"decision":"approve"}`); code != http.StatusOK {
		t.Fatalf("approve = %d", code)
	}
	awaitMember(t, done)
}

// A member the walk never arms leaves its record as it was.
func TestTeamMember_UnarmedMemberRecordsNoReview(t *testing.T) {
	h := newReviewHarness(t)
	ctx := teamrun.WithReviewArming(context.Background(), func(context.Context) bool { return false })
	res, err := h.srv.runTeamMember(ctx, "writer", teamrun.Prompt{Input: "go"}, "")
	if err != nil {
		t.Fatal(err)
	}
	run, _ := h.st.GetRun(context.Background(), res.RunID)
	if rec, _ := decodeRunConfig(run.RunConfig); rec.Review != nil {
		t.Errorf("record review = %v, want absent for a member never armed", *rec.Review)
	}
}
