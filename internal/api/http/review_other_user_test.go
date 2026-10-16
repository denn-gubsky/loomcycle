package http

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A reviewer's feedback is a turn the reviewer wrote: the run revises on it,
// so a reject with feedback from someone other than the run's user withholds
// the user's own credentials from the revision on. Approving adds nothing to
// the run and leaves it as it was.
func TestReview_FeedbackFromAnotherUserWithholdsTheRunUsersScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewer string
		withhold bool
	}{
		{"alice herself", "alice", false},
		{"bob", "bob", true},
	} {
		h := newReviewHarness(t)
		prov := &scopeRecorder{Provider: h.prov}
		h.srv.providers = &stubResolver{p: prov}
		runID, _, frames, stop := h.start(`{"agent":"writer","user_id":"alice","review":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"write the plan"}]}]}`)
		h.waitFrame(frames, "awaiting_review")
		h.waitHeld(runID, 1)
		reviewer := auth.WithPrincipal(context.Background(), auth.Principal{Subject: tc.reviewer, Scopes: []string{auth.ScopeTenant}})
		if _, err := h.srv.ReviewRun(reviewer, runID, "reject", "cover the rollback", "api"); err != nil {
			t.Fatalf("%s: reject: %v", tc.name, err)
		}
		h.waitHeld(runID, 2)
		calls := prov.calls()
		if len(calls) != 2 || calls[0].noUserScope || calls[1].noUserScope != tc.withhold {
			t.Errorf("%s: model calls %+v, want the first with the user's scope and the revision with no-user-scope=%t", tc.name, calls, tc.withhold)
		}
		stop()
	}
}
