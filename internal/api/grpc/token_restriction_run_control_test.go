package grpc

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A token limited to certain agents may not cancel, or answer the questions
// of, a run of another agent in its tenant over gRPC: the run is the NotFound
// an unknown one gets, and is left alone. seedRun files its runs under the
// agent "default", which the restricted token does not list.
func TestGrpcRunControl_RestrictedTokenCannotReachAnotherAgentsRun(t *testing.T) {
	adapter, httpSrv, st, _ := ownershipGRPC(t)
	seedRun(t, st, "acme", "dev", "a_default")
	run, err := st.GetRunByAgentID(context.Background(), "a_default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InterruptCreate(context.Background(), store.InterruptRow{
		InterruptID: "intr_grpc", RunID: run.ID, UserID: "dev", Question: "ok?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	live := liveCancel(t, httpSrv, st, "a_default")

	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate},
		Restrictions: auth.Restrictions{Agents: []string{"agentx"}},
	})
	if _, err := adapter.CancelAgent(restricted, &loomcyclepb.CancelAgentRequest{AgentId: "a_default"}); status.Code(err) != codes.NotFound {
		t.Errorf("restricted CancelAgent = %v, want NotFound", err)
	}
	if live() {
		t.Fatal("a token limited to another agent cancelled the run")
	}
	if _, err := adapter.ResolveInterrupt(restricted, &loomcyclepb.ResolveInterruptRequest{
		RunId: run.ID, InterruptId: "intr_grpc", Answer: "yes",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("restricted ResolveInterrupt = %v, want NotFound", err)
	}
	if row, err := st.InterruptGet(context.Background(), "intr_grpc"); err != nil || row.Status != store.InterruptStatusPending {
		t.Fatalf("the question after the refused answer: %+v (err %v), want it still pending", row, err)
	}

	free := scopedCtx("acme", "dev", auth.ScopeRunsCreate)
	if _, err := adapter.ResolveInterrupt(free, &loomcyclepb.ResolveInterruptRequest{
		RunId: run.ID, InterruptId: "intr_grpc", Answer: "yes",
	}); err != nil {
		t.Errorf("unrestricted ResolveInterrupt: %v", err)
	}
	if resp, err := adapter.CancelAgent(free, &loomcyclepb.CancelAgentRequest{AgentId: "a_default"}); err != nil || !live() {
		t.Errorf("unrestricted CancelAgent = %v, %v; want the run cancelled", resp, err)
	}
}
