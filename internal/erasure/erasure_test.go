package erasure_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func newSvc(t *testing.T) *erasure.Service {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &erasure.Service{Store: st}
}

func seed(t *testing.T, s *erasure.Service, tenant, subject string) {
	t.Helper()
	ctx := context.Background()
	st := s.Store
	sess, err := st.CreateSession(ctx, tenant, "chat", subject)
	if err != nil {
		t.Fatal(err)
	}
	prov := store.MemoryProvenance{Origin: "consolidator", SourceSessionID: sess.ID}
	v := json.RawMessage(`{"text":"x"}`)
	for _, w := range []struct {
		scope   store.MemoryScope
		scopeID string
	}{
		{store.MemoryScopeUser, subject},
		{store.MemoryScopeAgent, "curator"},
	} {
		if err := st.MemorySetProvenance(ctx, tenant, w.scope, w.scopeID, "memory/fact/a", v, 0, prov); err != nil {
			t.Fatalf("seed %s: %v", w.scope, err)
		}
	}
}

// TestService_ConfirmGuardLivesInTheService is the reason the logic was
// extracted rather than copied.
//
// The confirm requirement is a property of the ERASURE, not of HTTP. If each
// transport enforced it, the newest transport would be the one missing it — and
// the failure mode is deleting the wrong person, silently and irreversibly.
// Asserting it at the service means MCP, gRPC and every adapter inherit it
// without being able to opt out.
func TestService_ConfirmGuardLivesInTheService(t *testing.T) {
	s := newSvc(t)
	seed(t, s, "acme", "alice")
	ctx := context.Background()

	for _, tc := range []struct{ name, confirm string }{
		{"absent", ""},
		{"mismatched", "alicia"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Execute(ctx, erasure.ExecuteRequest{
				Tenant: "acme", Subject: "alice", DryRun: false, Confirm: tc.confirm,
			})
			if err != erasure.ErrConfirmMismatch {
				t.Fatalf("err = %v, want ErrConfirmMismatch — a transport must not be able "+
					"to reach a live erasure without it", err)
			}
		})
	}
	// And the refusals were inert.
	rep, err := s.Report(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tier1.Counts["memory_rows"] != 1 {
		t.Errorf("a refused erasure deleted data: memory_rows = %d, want 1",
			rep.Tier1.Counts["memory_rows"])
	}
}

// TestService_ResidueSurvivesAndIsReported pins the tier-3 contract every
// transport now shares: the residue is found, reported, and NOT deleted.
func TestService_ResidueSurvivesAndIsReported(t *testing.T) {
	s := newSvc(t)
	seed(t, s, "acme", "alice")
	ctx := context.Background()

	res, err := s.Execute(ctx, erasure.ExecuteRequest{
		Tenant: "acme", Subject: "alice", DryRun: false, Confirm: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Residue.Rows != 1 || len(res.Residue.Scopes) != 1 {
		t.Errorf("residue = %+v, want 1 row in one scope", res.Residue)
	}
	if _, ok := res.Retained["tier3_residue"]; !ok {
		t.Errorf("residue counted but not named in retained: %v", res.Retained)
	}
	// The agent-scope fact must still be there — residue is real, not bookkeeping.
	rows, _, _ := s.Store.MemoryList(ctx, "acme", store.MemoryScopeAgent, "curator", "", 0)
	if len(rows) != 1 {
		t.Errorf("agent-scope rows = %d, want 1 (must NOT be deleted)", len(rows))
	}
}

// TestService_PostErasureReportIsUndeterminableNotZero pins the one-shot
// property at the shared layer, so no transport can present the post-erasure
// zero as a clean result.
func TestService_PostErasureReportIsUndeterminableNotZero(t *testing.T) {
	s := newSvc(t)
	seed(t, s, "acme", "alice")
	ctx := context.Background()
	if _, err := s.Execute(ctx, erasure.ExecuteRequest{
		Tenant: "acme", Subject: "alice", DryRun: false, Confirm: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Report(ctx, "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tier3.Rows != 0 || rep.Tier3.SessionsExamined != 0 {
		t.Fatalf("precondition: want an untraceable post-erasure state, got %+v", rep.Tier3)
	}
	var warned bool
	for _, n := range rep.Notes {
		if contains(n, "UNDETERMINABLE") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("post-erasure report rendered residue 0 with no undeterminable warning — "+
			"a false all-clear while the fact is still stored; notes: %v", rep.Notes)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
