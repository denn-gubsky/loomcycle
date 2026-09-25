package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run's questions — a hook's included — are its author's to answer: the user
// is the main actor. An isolated member can neither see nor answer a question
// on a colleague's run, on any surface; the tenant check alone let it. A
// non-isolated colleague keeps the tenant's shared model.
func TestInterrupts_AnIsolatedMemberAnswersOnlyItsOwnRunsQuestions(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	sess, err := h.st.CreateSession(ctx, "acme", "writer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_alice", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	newAsk := func() string {
		id := store.MintInterruptID(time.Now())
		if _, err := h.st.InterruptCreate(ctx, store.InterruptRow{InterruptID: id, RunID: run.ID, Kind: store.InterruptKindQuestion,
			Status: store.InterruptStatusPending, Question: "Allow this call?", CreatedAt: time.Now(), UserID: "alice"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	as := func(subject string, scopes ...string) context.Context {
		return auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: subject, Scopes: scopes})
	}
	bob := as("bob", auth.ScopeRunsCreate, auth.ScopeUser)

	id := newAsk()
	if _, err := h.srv.ResolveInterrupt(bob, run.ID, id, "", "allow", "api", ""); !errors.Is(err, connector.ErrInterruptNotFound) {
		t.Errorf("bob resolved alice's question: %v", err)
	}
	if _, err := h.srv.InterruptionResolve(bob, connector.InterruptionResolveRequest{RunID: run.ID, InterruptID: id, Answer: "allow"}); err == nil {
		t.Error("bob resolved alice's question over MCP")
	}
	for name, req := range map[string]*http.Request{
		"run list":  httptest.NewRequest("GET", "/v1/runs/"+run.ID+"/interrupts", nil),
		"user list": httptest.NewRequest("GET", "/v1/users/alice/interrupts", nil),
	} {
		req.SetPathValue("run_id", run.ID)
		req.SetPathValue("user_id", "alice")
		rec := httptest.NewRecorder()
		if name == "run list" {
			h.srv.handleListRunInterrupts(rec, req.WithContext(bob))
		} else {
			h.srv.handleListUserInterrupts(rec, req.WithContext(bob))
		}
		var out struct {
			Total int `json:"total"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK || out.Total != 0 {
			t.Errorf("bob's %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if row, _ := h.st.InterruptGet(ctx, id); row.Status != store.InterruptStatusPending {
		t.Fatalf("the question was answered: %q", row.Status)
	}

	if st, err := h.srv.ResolveInterrupt(as("alice", auth.ScopeRunsCreate, auth.ScopeUser), run.ID, id, "", "allow", "api", ""); err != nil || st != string(store.InterruptStatusResolved) {
		t.Errorf("alice answering her own run: %q, %v", st, err)
	}
	carol := newAsk()
	if _, err := h.srv.ResolveInterrupt(as("carol", auth.ScopeRunsCreate), run.ID, carol, "", "allow", "api", ""); err != nil {
		t.Errorf("a non-isolated colleague: %v", err)
	}
}
