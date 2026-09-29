package http

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/breakpoints"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func reviewTTLFrom(t *testing.T, body string) int {
	t.Helper()
	var out struct {
		ReviewTTLSeconds *int `json:"review_ttl_seconds"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if out.ReviewTTLSeconds == nil {
		t.Fatalf("no review_ttl_seconds in %s", body)
	}
	return *out.ReviewTTLSeconds
}

// TestHandleBreakpoints_GetReportsTheReviewDeadline: the walk's deadline is part
// of its arming, so reading the arming back says what it is.
func TestHandleBreakpoints_GetReportsTheReviewDeadline(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	_, release, _ := srv.breakpointReg.Open(runID, []string{"wave:review"}, 90*time.Second, nil)
	defer release()

	rec := doJSON(t, srv, "GET", "/v1/runs/"+runID+"/breakpoints", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := reviewTTLFrom(t, rec.Body.String()); got != 90 {
		t.Errorf("review_ttl_seconds = %d, want the walk's 90", got)
	}
}

// TestHandleBreakpoints_PutChangesTheReviewDeadline: sent with the set, the
// deadline is replaced — 0 is no deadline, as at the start of a walk.
func TestHandleBreakpoints_PutChangesTheReviewDeadline(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, _ := srv.breakpointReg.Open(runID, []string{"wave:review"}, 90*time.Second, nil)
	defer release()

	for _, tc := range []struct {
		send int
		want time.Duration
	}{{3600, time.Hour}, {0, 0}} {
		body := `{"breakpoints":["wave:review"],"review_ttl_seconds":` + strconv.Itoa(tc.send) + `}`
		rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", body)
		if rec.Code != 200 {
			t.Fatalf("PUT %s = %d; body=%s", body, rec.Code, rec.Body.String())
		}
		if got := reviewTTLFrom(t, rec.Body.String()); got != tc.send {
			t.Errorf("PUT %d reported %d", tc.send, got)
		}
		if set.ReviewTTL() != tc.want {
			t.Errorf("after PUT %d the walk's deadline is %v, want %v", tc.send, set.ReviewTTL(), tc.want)
		}
	}
}

// TestHandleBreakpoints_PutWithoutADeadlineLeavesItUnchanged: a caller re-arming
// the specs has not asked for the deadline to go.
func TestHandleBreakpoints_PutWithoutADeadlineLeavesItUnchanged(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release, _ := srv.breakpointReg.Open(runID, []string{"wave:review"}, 90*time.Second, nil)
	defer release()

	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wave"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := reviewTTLFrom(t, rec.Body.String()); got != 90 || set.ReviewTTL() != 90*time.Second {
		t.Errorf("reported %d, set holds %v — want the 90s deadline kept", got, set.ReviewTTL())
	}
}

// TestHandleBreakpoints_PutRefusesAnInvalidDeadlineAndKeepsTheArming: a deadline
// the walk could not honour is refused, and so is the set sent with it — a
// refused call changes neither, just as a refused spec changes neither.
func TestHandleBreakpoints_PutRefusesAnInvalidDeadlineAndKeepsTheArming(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	runID := seedRun(t, srv)
	set, release := openWalkOnTargetsTeamWithTTL(t, srv, runID, []string{"wave:review"}, 90*time.Second)
	defer release()

	for _, body := range []string{
		`{"breakpoints":["wave"],"review_ttl_seconds":-1}`,
		// Past time.Duration's range, which would wrap to "no deadline".
		`{"breakpoints":["wave"],"review_ttl_seconds":9223372037}`,
	} {
		rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", body)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_review_ttl") {
			t.Errorf("PUT %s = %d, want 400 invalid_review_ttl; body=%s", body, rec.Code, rec.Body.String())
		}
	}
	// A valid deadline sent with a spec the walk cannot arm is refused with it.
	rec := doJSON(t, srv, "PUT", "/v1/runs/"+runID+"/breakpoints", `{"breakpoints":["wvae"],"review_ttl_seconds":60}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_breakpoint") {
		t.Errorf("PUT with an unknown state = %d, want 400 invalid_breakpoint; body=%s", rec.Code, rec.Body.String())
	}
	if set.ReviewTTL() != 90*time.Second || !set.Armed("wave", breakpoints.Review) || set.Armed("wave", breakpoints.BeforeDispatch) {
		t.Errorf("a refused PUT changed the arming: ttl=%v armed=%v", set.ReviewTTL(), set.List())
	}
}

// openWalkOnTargetsTeamWithTTL is openWalkOnTargetsTeam for a walk started with
// a review deadline.
func openWalkOnTargetsTeamWithTTL(t *testing.T, srv *Server, runID string, seed []string, ttl time.Duration) (*breakpoints.Set, func()) {
	t.Helper()
	set, release := openWalkOnTargetsTeam(t, srv, runID, seed)
	d := ttl
	if err := set.Apply(seed, &d); err != nil {
		t.Fatal(err)
	}
	return set, release
}

// A deadline changed through the endpoint applies to the member's NEXT hold: the
// revision it holds after feedback carries the new deadline.
func TestBreakpoints_AChangedDeadlineAppliesToTheMembersNextHold(t *testing.T) {
	h := newReviewHarness(t)
	walkRunID, set := walkUnderReviewWithTTL(t, h, time.Hour, "draft:review")
	member, done := startWalkMember(t, h, walkRunID, set, "draft")
	if got := h.heldUntil(member); got != time.Hour {
		t.Fatalf("round 1 deadline = %v, want the walk's 1h", got)
	}

	h.putBreakpoints(walkRunID, `{"breakpoints":["draft:review"],"review_ttl_seconds":7200}`)
	if code, body := h.review(member, `{"decision":"reject","feedback":"cover the rollback"}`); code != 200 {
		t.Fatalf("reject = %d %s", code, body)
	}
	h.waitHeld(member, 2)
	if got := h.heldUntil(member); got != 2*time.Hour {
		t.Errorf("round 2 deadline = %v, want the 2h set while round 1 was held", got)
	}
	if code, body := h.review(member, `{"decision":"approve"}`); code != 200 {
		t.Fatalf("approve = %d %s", code, body)
	}
	awaitMember(t, done)
}

// A hold in progress keeps the deadline it announced when the endpoint changes
// the walk's: the member still ends rejected on it, though the walk now says an
// hour.
func TestBreakpoints_AHoldInProgressKeepsItsDeadlineAfterAPut(t *testing.T) {
	h := newReviewHarness(t)
	walkRunID, set := walkUnderReviewWithTTL(t, h, 2*time.Second, "draft:review")
	_, done := startWalkMember(t, h, walkRunID, set, "draft")

	h.putBreakpoints(walkRunID, `{"breakpoints":["draft:review"],"review_ttl_seconds":3600}`)
	if set.ReviewTTL() != time.Hour {
		t.Fatalf("the PUT did not change the walk's deadline: %v", set.ReviewTTL())
	}
	select {
	case res := <-done:
		if res.Status != string(store.RunRejected) {
			t.Fatalf("member = %+v, want rejected on the deadline its hold began with", res)
		}
		run, _ := h.st.GetRun(context.Background(), res.RunID)
		if run.StopReason != "review_expired" {
			t.Errorf("stop reason = %q, want review_expired", run.StopReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hold did not expire on the deadline it announced")
	}
}
