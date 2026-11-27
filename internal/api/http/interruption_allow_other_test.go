package http

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC HG P1: a question with options may allow an answer of the person's own
// (allow_other). HTTP / gRPC (ResolveInterrupt) and MCP (InterruptionResolve)
// apply one check, connector.CheckInterruptAnswer, with one refusal.

func seedOptionsInterrupt(t *testing.T, st store.Store, agent string, allowOther bool) (runID, interruptID string) {
	t.Helper()
	runID = seedRunInTenant(t, st, "acme", "alice", agent)
	interruptID = store.MintInterruptID(time.Now())
	if _, err := st.InterruptCreate(context.Background(), store.InterruptRow{
		InterruptID: interruptID, RunID: runID, UserID: "alice",
		Question: "Proceed?", Options: []byte(`["Yes","No"]`), AllowOther: allowOther, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}
	return runID, interruptID
}

func TestHandleResolveInterrupt_AllowOtherTakesAnAnswerOutsideTheOptions(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID, id := seedOptionsInterrupt(t, st, "a_acme_hg1", true)

	rr := resolveReq(t, s, runID, id, `{"answer":"Neither, wait a week"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q, want 200", rr.Code, rr.Body.String())
	}
	row, _ := st.InterruptGet(context.Background(), id)
	if row.Status != store.InterruptStatusResolved || row.Answer != "Neither, wait a week" {
		t.Errorf("stored (%q, %q), want (resolved, the person's own answer)", row.Status, row.Answer)
	}
}

func TestHandleResolveInterrupt_AllowOtherStillRefusesAnEmptyOrOverlongAnswer(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID, id := seedOptionsInterrupt(t, st, "a_acme_hg2", true)

	long := strings.Repeat("x", connector.MaxInterruptOtherAnswerChars+1)
	for _, body := range []string{`{"answer":""}`, `{"answer":"` + long + `"}`} {
		if rr := resolveReq(t, s, runID, id, body); rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("body of %d bytes: status=%d, want 422", len(body), rr.Code)
		}
	}
	if row, _ := st.InterruptGet(context.Background(), id); row.Status != store.InterruptStatusPending {
		t.Errorf("status = %q after refused answers, want pending", row.Status)
	}
}

func TestHandleResolveInterrupt_OptionsWithoutAllowOtherRefuseAnAnswerOutsideThem(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID, id := seedOptionsInterrupt(t, st, "a_acme_hg3", false)

	if rr := resolveReq(t, s, runID, id, `{"answer":"Maybe"}`); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=%d, want 422", rr.Code)
	}
}

// TestInterruptionResolve_MCPSharesTheResolveCheck pins that the MCP path
// takes the same check as HTTP: allow_other lets an answer outside the options
// through, and a refusal is ErrInterruptInvalidAnswer with the HTTP message
// (the MCP copy used to be a plain error with its own wording).
func TestInterruptionResolve_MCPSharesTheResolveCheck(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := tenantPrincipalCtx("acme", "alice", auth.ScopeRunsCreate)

	runID, id := seedOptionsInterrupt(t, st, "a_acme_hg4", false)
	_, err := s.InterruptionResolve(ctx, connector.InterruptionResolveRequest{RunID: runID, InterruptID: id, Answer: "Maybe"})
	if !errors.Is(err, connector.ErrInterruptInvalidAnswer) {
		t.Fatalf("err = %v, want ErrInterruptInvalidAnswer", err)
	}
	want := connector.CheckInterruptAnswer(store.InterruptRow{Options: []byte(`["Yes","No"]`)}, "Maybe").Error()
	if err.Error() != want {
		t.Errorf("message = %q, want the shared %q", err.Error(), want)
	}

	runID, id = seedOptionsInterrupt(t, st, "a_acme_hg5", true)
	if _, err := s.InterruptionResolve(ctx, connector.InterruptionResolveRequest{RunID: runID, InterruptID: id, Answer: "Maybe"}); err != nil {
		t.Fatalf("allow_other answer refused: %v", err)
	}
	if row, _ := st.InterruptGet(context.Background(), id); row.Answer != "Maybe" {
		t.Errorf("stored answer = %q, want Maybe", row.Answer)
	}
}
