package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// These regression tests pin RFC BH P2 — the "declined" disposition on the
// resolve endpoint. A decline resolves a pending question WITHOUT an answer
// (skips option validation) and writes the new terminal `declined` status so
// the waiting Question tool proceeds. They fail on the pre-P2 handler (which
// had no disposition field: a decline body fell through to the answer path and
// 422'd on the missing/invalid answer, and InterruptFinish rejected the status).

// seedPendingInterruptWithOptions creates a pending question with declared
// options in the given tenant and returns run_id + interrupt_id.
func seedPendingInterruptWithOptions(t *testing.T, st store.Store, tenant, user, agent string) (runID, interruptID string) {
	t.Helper()
	runID = seedRunInTenant(t, st, tenant, user, agent)
	id := store.MintInterruptID(time.Now())
	if _, err := st.InterruptCreate(context.Background(), store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: user,
		Question: "Proceed?", Options: []byte(`["Yes","No"]`), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}
	return runID, id
}

// resolveReq drives handleResolveInterrupt with the given JSON body as an
// own-tenant (acme/alice) caller and returns the recorder.
func resolveReq(t *testing.T, s *Server, runID, interruptID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost,
		"/v1/runs/"+runID+"/interrupts/"+interruptID+"/resolve",
		strings.NewReader(body))
	r.SetPathValue("run_id", runID)
	r.SetPathValue("interrupt_id", interruptID)
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(tenantPrincipalCtx("acme", "alice", auth.ScopeRunsCreate))
	rr := httptest.NewRecorder()
	s.handleResolveInterrupt(rr, r)
	return rr
}

func TestHandleResolveInterrupt_DeclineSkipsOptionValidationAndWakes(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	s.interruptionBus = channels.NewBus()
	runID, id := seedPendingInterruptWithOptions(t, st, "acme", "alice", "a_acme_dec")

	// A waiter registered before the resolve proves the handler fires the
	// same wake for a decline as it does for an answer.
	waker := s.interruptionBus.Register("intr:" + id)

	// Decline with NO answer, against an interrupt that DECLARES options —
	// option validation is skipped entirely for a decline.
	rr := resolveReq(t, s, runID, id, `{"disposition":"declined"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("decline status=%d body=%q, want 200", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"declined"`) {
		t.Errorf("response = %q, want status=declined", rr.Body.String())
	}

	row, _ := st.InterruptGet(context.Background(), id)
	if row.Status != store.InterruptStatusDeclined {
		t.Errorf("stored status = %q, want declined", row.Status)
	}
	if row.Answer != "" {
		t.Errorf("stored answer = %q, want empty on decline", row.Answer)
	}
	if row.ResolvedBy != store.InterruptResolvedByAPI {
		t.Errorf("resolved_by = %q, want api (bearer caller)", row.ResolvedBy)
	}

	select {
	case <-waker:
	case <-time.After(time.Second):
		t.Error("decline did not fire the interruptionBus wake")
	}
}

func TestHandleResolveInterrupt_DeclineOnAlreadyTerminalConflicts(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID, id := seedPendingInterruptWithOptions(t, st, "acme", "alice", "a_acme_dec2")
	// Already resolved → a subsequent decline must 409, not silently succeed.
	if err := st.InterruptResolve(context.Background(), id, "Yes", store.InterruptResolvedByWebUI, nil); err != nil {
		t.Fatalf("InterruptResolve: %v", err)
	}

	rr := resolveReq(t, s, runID, id, `{"disposition":"declined"}`)
	if rr.Code != http.StatusConflict {
		t.Errorf("decline of terminal interrupt status=%d, want 409", rr.Code)
	}
}

func TestHandleResolveInterrupt_UnknownDispositionRejected(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	// Free-text interrupt (no options) + a valid non-empty answer: the ONLY
	// thing that can 422 here is the unknown-disposition gate, so this fails
	// on the pre-P2 handler (which ignored disposition and would resolve).
	runID := seedRunInTenant(t, st, "acme", "alice", "a_acme_dec3")
	id := store.MintInterruptID(time.Now())
	if _, err := st.InterruptCreate(context.Background(), store.InterruptRow{
		InterruptID: id, RunID: runID, UserID: "alice", Question: "free?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}

	rr := resolveReq(t, s, runID, id, `{"disposition":"skip","answer":"whatever"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown disposition status=%d, want 422", rr.Code)
	}
	if row, _ := st.InterruptGet(context.Background(), id); row.Status != store.InterruptStatusPending {
		t.Errorf("interrupt status changed to %q on rejected disposition", row.Status)
	}
}

func TestHandleResolveInterrupt_DeclineWithAnswerRejected(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	runID, id := seedPendingInterruptWithOptions(t, st, "acme", "alice", "a_acme_dec4")

	// A decline carries no answer; an answer alongside it is contradictory.
	rr := resolveReq(t, s, runID, id, `{"disposition":"declined","answer":"Yes"}`)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("decline+answer status=%d, want 422", rr.Code)
	}
	if row, _ := st.InterruptGet(context.Background(), id); row.Status != store.InterruptStatusPending {
		t.Errorf("interrupt status changed to %q on rejected decline+answer", row.Status)
	}
}

// TestHandleResolveInterrupt_AnswerPathUnchanged is the byte-identical
// regression guard: with no disposition (or "answer"), the existing answer
// path is unchanged — a valid option answers (200, status=resolved) and an
// out-of-options answer still 422s (option validation stays active).
func TestHandleResolveInterrupt_AnswerPathUnchanged(t *testing.T) {
	s, st := tokenAuthServer(t, "")

	runID, id := seedPendingInterruptWithOptions(t, st, "acme", "alice", "a_acme_ans1")
	if rr := resolveReq(t, s, runID, id, `{"answer":"Yes"}`); rr.Code != http.StatusOK ||
		!strings.Contains(rr.Body.String(), `"status":"resolved"`) {
		t.Errorf("valid answer status=%d body=%q, want 200 status=resolved", rr.Code, rr.Body.String())
	}
	if row, _ := st.InterruptGet(context.Background(), id); row.Status != store.InterruptStatusResolved || row.Answer != "Yes" {
		t.Errorf("answered row = {%q,%q}, want {resolved,Yes}", row.Status, row.Answer)
	}

	// Out-of-options answer on the answer path still 422s (validation active).
	runID2, id2 := seedPendingInterruptWithOptions(t, st, "acme", "alice", "a_acme_ans2")
	if rr := resolveReq(t, s, runID2, id2, `{"answer":"Maybe"}`); rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("out-of-options answer status=%d, want 422", rr.Code)
	}
}
