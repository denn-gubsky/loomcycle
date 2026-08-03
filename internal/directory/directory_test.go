package directory_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/directory"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func newSvc(t *testing.T) *directory.Service {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &directory.Service{Store: st}
}

// seed creates a run (and its session) for one subject in one tenant.
func seed(t *testing.T, s *directory.Service, tenant, subject string) {
	t.Helper()
	ctx := context.Background()
	sess, err := s.Store.CreateSession(ctx, tenant, "chat", subject)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := s.Store.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: "a_dir", UserID: subject, TenantID: tenant,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

// TestInspect_IsConfinedToTheGivenTenant is the property the whole service exists
// to get right once instead of five times.
//
// Inspect aggregates five surfaces, each with its own tenant-scoping rule. Before
// this, an operator answering "what does alice have" made five calls and had to get
// the scoping right in all five. The failure mode of getting one wrong is not an
// error — it is a plausible number from the wrong tenant.
func TestInspect_IsConfinedToTheGivenTenant(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	const subject = "alice"
	// The same subject id exists in BOTH tenants, which is legal: a subject is only
	// unique within a tenant.
	seed(t, s, "acme", subject)
	seed(t, s, "globex", subject)
	seed(t, s, "globex", subject)

	// And a memory row that belongs only to globex's alice.
	v, _ := json.Marshal("globex-only")
	if err := s.Store.MemorySet(ctx, "globex", store.MemoryScopeUser, subject, "memory/fact/x", v, 0); err != nil {
		t.Fatal(err)
	}

	acme, err := s.Inspect(ctx, "acme", subject)
	if err != nil {
		t.Fatal(err)
	}
	if len(acme.Errors) > 0 {
		t.Fatalf("planes faulted: %v", acme.Errors)
	}
	if acme.Activity.TotalCount != 1 {
		t.Errorf("acme activity total = %d, want 1 — globex's runs leaked in",
			acme.Activity.TotalCount)
	}
	if acme.Chats != 1 {
		t.Errorf("acme chats = %d, want 1", acme.Chats)
	}
	if got := acme.Memory["user_scope_rows"]; got != 0 {
		t.Errorf("acme memory rows = %d, want 0 — the row belongs to globex's alice", got)
	}

	globex, err := s.Inspect(ctx, "globex", subject)
	if err != nil {
		t.Fatal(err)
	}
	if globex.Activity.TotalCount != 2 || globex.Chats != 2 {
		t.Errorf("globex = %d runs / %d chats, want 2/2", globex.Activity.TotalCount, globex.Chats)
	}
	if got := globex.Memory["user_scope_rows"]; got != 1 {
		t.Errorf("globex memory rows = %d, want 1", got)
	}
}

// TestInspect_RequiresASubject — an empty subject must be refused, not answered
// with a zero-filled view that reads as "this person has nothing".
func TestInspect_RequiresASubject(t *testing.T) {
	s := newSvc(t)
	if _, err := s.Inspect(context.Background(), "acme", ""); err != directory.ErrNoSubject {
		t.Fatalf("err = %v, want ErrNoSubject", err)
	}
}

// TestInspect_UnexaminedDocumentPlaneIsDeclared — with SqlMem nil the document
// plane is NOT examined, and saying nothing would be indistinguishable from
// "examined, found none". Same rule the erasure report follows.
func TestInspect_UnexaminedDocumentPlaneIsDeclared(t *testing.T) {
	s := newSvc(t) // SqlMem is nil
	seed(t, s, "acme", "alice")
	ins, err := s.Inspect(context.Background(), "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ins.Documents != nil {
		t.Errorf("documents reported as %v with no SQL Memory configured", *ins.Documents)
	}
	var declared bool
	for _, n := range ins.Notes {
		if len(n) > 0 && contains(n, "NOT examined") {
			declared = true
		}
	}
	if !declared {
		t.Errorf("the unexamined document plane was not declared, so its absence reads "+
			"as zero; notes: %v", ins.Notes)
	}
}

// TestTenants_CountsDistinctSubjects — the tenant list is derived, so its user
// count must be distinct SUBJECTS rather than run rows, and a tenant with no runs
// must be absent rather than reported empty.
func TestTenants_CountsDistinctSubjects(t *testing.T) {
	s := newSvc(t)
	seed(t, s, "acme", "u_a")
	seed(t, s, "acme", "u_a")
	seed(t, s, "acme", "u_b")
	seed(t, s, "globex", "u_c")

	rows, err := s.Tenants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]directory.TenantRow{}
	for _, r := range rows {
		got[r.Tenant] = r
	}
	if a := got["acme"]; a.Users != 2 || a.Runs != 3 {
		t.Errorf("acme = %d users / %d runs, want 2/3 (distinct subjects, all runs)", a.Users, a.Runs)
	}
	if _, present := got["never_ran"]; present {
		t.Error("a tenant with no runs appeared — the list is derived from runs")
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
