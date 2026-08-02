package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestErasureReport_CountsResidueOutsideTheSubjectsOwnScope is the behaviour the
// whole report exists for.
//
// A subject-keyed delete finds the fact in the subject's OWN user scope and stops.
// It cannot find a fact ABOUT them that a shared agent filed under itself, or that
// another user's scope holds — those rows are not keyed to the subject at all, and
// the only thing connecting them is the chat they were derived from.
//
// So the assertion is not "the report returns numbers". It is that the two
// unreachable rows are counted SEPARATELY from the reachable one, and that the
// scopes holding them are named. A report that summed all three would read as
// "3 rows, all deletable" — precisely the false-completeness this is built to
// prevent.
func TestErasureReport_CountsResidueOutsideTheSubjectsOwnScope(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ctx := context.Background()
	const tenant, subject = "acme", "alice"

	// A chat owned by the subject: the handle every derived fact is traced from.
	sess, err := srv.store.CreateSession(ctx, tenant, "chat", subject)
	if err != nil {
		t.Fatal(err)
	}
	prov := store.MemoryProvenance{Origin: "consolidator", SourceSessionID: sess.ID}
	val := json.RawMessage(`{"text":"alice prefers async review"}`)

	// Three facts, all derived from that one chat, differing only in WHERE they
	// were filed — which is the entire difference between reachable and not.
	for _, w := range []struct {
		scope   store.MemoryScope
		scopeID string
	}{
		{store.MemoryScopeUser, subject},    // tier 1 — the subject's own scope
		{store.MemoryScopeAgent, "curator"}, // tier 3 — a shared agent's scope
		{store.MemoryScopeUser, "bob"},      // tier 3 — ANOTHER user's scope
	} {
		if err := srv.store.MemorySetProvenance(ctx, tenant, w.scope, w.scopeID,
			"memory/fact/alice-review", val, 0, prov); err != nil {
			t.Fatalf("seed %s/%s: %v", w.scope, w.scopeID, err)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant="+tenant+"&subject="+subject, nil).
		WithContext(auth.WithPrincipal(ctx, auth.Principal{
			TenantID: tenant, Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleErasureReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var rep erasure.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Errors) > 0 {
		t.Fatalf("planes failed to read, so every count is a lower bound: %v", rep.Errors)
	}

	// The subject's own fact is reachable, and is NOT residue.
	if got := rep.Tier1.Counts["memory_rows"]; got != 1 {
		t.Errorf("tier1 memory_rows = %d, want 1 (the fact in the subject's own scope)", got)
	}
	// The other two are residue: a subject-keyed delete leaves them behind.
	if rep.Tier3.Rows != 2 {
		t.Errorf("tier3 rows = %d, want 2 (agent:curator + user:bob); "+
			"a residue count that misses these makes the report claim completeness it does not have",
			rep.Tier3.Rows)
	}
	// And the report must say WHERE, or an operator cannot act on it.
	want := map[string]bool{"agent:curator": true, "user:bob": true}
	for _, s := range rep.Tier3.Scopes {
		delete(want, s)
	}
	if len(want) > 0 {
		t.Errorf("tier3 scopes = %v, missing %v", rep.Tier3.Scopes, want)
	}
	if rep.Tier3.SessionsExamined != 1 {
		t.Errorf("sessions_examined = %d, want 1", rep.Tier3.SessionsExamined)
	}
}

// TestErasureReport_UnreadablePlaneIsReportedNotSilentlyZero locks the failure
// mode that would make this report dangerous.
//
// Every plane read is best-effort so one broken store does not deny the whole
// answer. The hazard in that choice is that a plane which ERRORS looks exactly
// like a plane that is EMPTY — and "we hold nothing about you" derived from a
// failed query is the worst possible wrong answer here.
func TestErasureReport_UnreadablePlaneIsReportedNotSilentlyZero(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ctx := context.Background()

	// Close the store out from under the handler so its reads fault.
	if closer, ok := srv.store.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	} else {
		t.Skip("store has no Close; cannot simulate a read fault")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant=acme&subject=alice", nil).
		WithContext(auth.WithPrincipal(ctx, auth.Principal{
			TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleErasureReport(rec, req)

	var rep erasure.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Errors) == 0 {
		t.Fatalf("a report built from failed reads returned no errors — "+
			"an all-zero report would be indistinguishable from 'nothing is held'; body: %s",
			rec.Body.String())
	}
	var sawBound bool
	for _, n := range rep.Notes {
		if len(n) > 0 && containsLowerBound(n) {
			sawBound = true
		}
	}
	if !sawBound {
		t.Errorf("errors were reported but no note warns the counts are a lower bound: %v", rep.Notes)
	}
}

// TestErasureReport_AdminMustNameTheTenant locks the refusal.
//
// Everywhere else in this API an admin omitting ?tenant= means "all tenants". Here
// it cannot: `all` carries tenant "", which every store read would answer for the
// DEFAULT tenant — producing a confident report about a different set of people
// than the one asked about. Refusing is the only answer that cannot be wrong.
func TestErasureReport_AdminMustNameTheTenant(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	adminCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_erasure?subject=alice", nil).WithContext(adminCtx)
	srv.handleErasureReport(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("omitted tenant: status = %d, want 400 — an unnamed tenant would be "+
			"silently answered as the default one; body: %s", rec.Code, rec.Body.String())
	}

	// An EXPLICIT empty tenant is a real request: on a single-tenant install the
	// default tenant is the whole deployment, and refusing it would lock the
	// admin out of their own data.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant=&subject=alice", nil).WithContext(adminCtx)
	srv.handleErasureReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit ?tenant=: status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func containsLowerBound(s string) bool {
	const want = "LOWER BOUND"
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}

// TestErasureReport_EveryExaminedPlaneIsCountedEvenAtZero pins the invariant that
// a key's PRESENCE means the plane was examined.
//
// The regression this guards: sql_memory_scopes was set only when a scope was
// found, so its absence meant either "no scope" or "never looked". That is the
// same ambiguity between failed and empty that `errors` exists to prevent,
// reintroduced one field lower down — and it is worse in a compliance report than
// anywhere else, because the reader's question is precisely "is this plane clear?"
func TestErasureReport_EveryExaminedPlaneIsCountedEvenAtZero(t *testing.T) {
	// ontologyFixture rather than makeServer: it wires a REAL sqlMem, and the
	// regression being guarded lives entirely inside the `s.sqlMem != nil` branch.
	// With a nil sqlMem the assertion below passes through the "declared
	// unexamined" path and never executes the fixed code — a vacuous guard, which
	// is how the first version of this test passed against the unfixed report.
	srv, _ := ontologyFixture(t)
	adminCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant=acme&subject=nobody", nil).
		WithContext(adminCtx)
	srv.handleErasureReport(rec, req)
	var rep erasure.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A subject with nothing stored must still account for every plane examined.
	for _, k := range []string{"chats", "memory_rows", "path_entries"} {
		if _, ok := rep.Tier1.Counts[k]; !ok {
			t.Errorf("tier1 %q absent for an empty subject — absence cannot be "+
				"distinguished from 'never examined'; counts: %v", k, rep.Tier1.Counts)
		}
	}
	for _, k := range []string{"credentials", "token_limits", "interrupts", "usage_ledger_calls"} {
		if _, ok := rep.Tier2.Counts[k]; !ok {
			t.Errorf("tier2 %q absent for an empty subject; counts: %v", k, rep.Tier2.Counts)
		}
	}

	// SQL Memory is the conditional one: either it was examined and reported a
	// count, or it does not exist here and the notes must SAY so. Silence is the
	// one outcome that is not allowed.
	_, counted := rep.Tier1.Counts["sql_memory_scopes"]
	var declared bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "SQL Memory is not configured") {
			declared = true
		}
	}
	if counted == declared {
		t.Errorf("sql_memory must be either counted (%v) or declared unexamined (%v), "+
			"exactly one; counts=%v notes=%v", counted, declared, rep.Tier1.Counts, rep.Notes)
	}
}
