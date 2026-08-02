package erasure_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
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

// newSvcWithSQL builds a service with a real SQL Memory manager.
func newSvcWithSQL(t *testing.T) *erasure.Service {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return &erasure.Service{Store: st, SqlMem: mgr}
}

// TestService_DefaultTenantSQLScopeIsActuallyDropped is the regression for the
// nastiest shape this subsystem produces: the same tenant spelled two ways.
//
// SQL Memory REJECTS an empty tenant and stores the default one as "default";
// the k/v plane and the Path tree keep the RAW "". An erasure that built its
// DropScope key from the raw tenant therefore matched nothing on a
// single-tenant deployment — the most common install — and left the subject's
// ENTIRE SQL Memory database (documents, entity graph) in place while reporting
// success.
//
// The live deployment test could not catch this: its tenant was "test-mem",
// where raw and canonical are identical. Only the default tenant diverges.
func TestService_DefaultTenantSQLScopeIsActuallyDropped(t *testing.T) {
	s := newSvcWithSQL(t)
	ctx := context.Background()
	const subject = "alice"
	// The raw tenant is "" — SQL Memory stores it as "default".
	key := sqlmem.ScopeKey{Tenant: "default", Scope: "user", ScopeID: subject}
	if _, err := s.SqlMem.Exec(ctx, key, `CREATE TABLE notes (body TEXT)`, nil, 0); err != nil {
		t.Fatalf("provision scope: %v", err)
	}

	rep, err := s.Report(ctx, "", subject)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tier1.Counts["sql_memory_scopes"] != 1 {
		t.Fatalf("report missed the default-tenant SQL scope: counts=%v", rep.Tier1.Counts)
	}

	res, err := s.Execute(ctx, erasure.ExecuteRequest{
		Tenant: "", Subject: subject, DryRun: false, Confirm: subject,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted["sql_memory_scopes"] != 1 {
		t.Errorf("deleted sql_memory_scopes = %d, want 1; deleted=%v errors=%v",
			res.Deleted["sql_memory_scopes"], res.Deleted, res.Errors)
	}
	// Proven independently of the erasure's own account of itself.
	scopes, err := s.SqlMem.ListScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range scopes {
		if sk.Tenant == "default" && sk.Scope == "user" && sk.ScopeID == subject {
			t.Errorf("the subject's SQL Memory database SURVIVED the erasure (%+v) — "+
				"documents and entity graph still readable", sk)
		}
	}
}

// TestService_SQLScopeLookupIsTenantScoped closes the cross-tenant oracle.
//
// ListScopes spans every tenant, so matching on scope+scope_id alone let tenant
// A learn that tenant B has a user named X — and produced a dry run promising to
// delete a database the live path could never reach.
func TestService_SQLScopeLookupIsTenantScoped(t *testing.T) {
	s := newSvcWithSQL(t)
	ctx := context.Background()
	const subject = "alice"
	// alice exists ONLY in globex.
	key := sqlmem.ScopeKey{Tenant: "globex", Scope: "user", ScopeID: subject}
	if _, err := s.SqlMem.Exec(ctx, key, `CREATE TABLE notes (body TEXT)`, nil, 0); err != nil {
		t.Fatalf("provision: %v", err)
	}

	rep, err := s.Report(ctx, "acme", subject)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Tier1.Counts["sql_memory_scopes"]; got != 0 {
		t.Errorf("acme's report saw globex's SQL scope for the same subject id (got %d) — "+
			"a cross-tenant existence oracle, and a dry run the live path cannot honour", got)
	}

	// And the dry run agrees with the live path.
	dry, err := s.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: subject, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted["sql_memory_scopes"] != 0 {
		t.Errorf("dry run promised to delete another tenant's scope: %v", dry.Deleted)
	}
	// globex's database is untouched.
	scopes, _ := s.SqlMem.ListScopes(ctx)
	var still bool
	for _, sk := range scopes {
		if sk.Tenant == "globex" && sk.ScopeID == subject {
			still = true
		}
	}
	if !still {
		t.Error("globex's SQL Memory scope was destroyed by acme's erasure")
	}
}
