package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// erasePost drives POST /v1/_erasure and decodes the result.
func erasePost(t *testing.T, srv *Server, tenant string, body map[string]any) (*httptest.ResponseRecorder, erasureResult) {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_erasure?tenant="+tenant, bytes.NewReader(b)).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: tenant, Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleErasureExecute(rec, req)
	var res erasureResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return rec, res
}

// seedSubject plants one fact in the subject's own scope and one in a shared
// agent's, both derived from a chat the subject owns.
func seedSubject(t *testing.T, srv *Server, tenant, subject string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, tenant, "chat", subject)
	if err != nil {
		t.Fatal(err)
	}
	prov := store.MemoryProvenance{Origin: "consolidator", SourceSessionID: sess.ID}
	val := json.RawMessage(`{"text":"alice lives at 12 Elm St"}`)
	for _, w := range []struct {
		scope   store.MemoryScope
		scopeID string
	}{
		{store.MemoryScopeUser, subject},
		{store.MemoryScopeAgent, "curator"},
	} {
		if err := srv.store.MemorySetProvenance(ctx, tenant, w.scope, w.scopeID,
			"memory/fact/addr", val, 0, prov); err != nil {
			t.Fatalf("seed %s: %v", w.scope, err)
		}
	}
	return sess.ID
}

// TestErasureExecute_DefaultsToDryRun locks the safety default.
//
// An omitted dry_run must mean "do nothing". The field is a *bool precisely so
// the zero value cannot be destructive: with a plain bool, POST {} — a caller who
// forgot the field, or a client with a buggy serializer — would erase a person.
func TestErasureExecute_DefaultsToDryRun(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	const tenant, subject = "acme", "alice"
	seedSubject(t, srv, tenant, subject)

	rec, res := erasePost(t, srv, tenant, map[string]any{"subject": subject})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !res.DryRun {
		t.Fatal("an omitted dry_run executed a LIVE erasure — the default must be inert")
	}
	if res.Deleted["memory_rows"] != 1 {
		t.Errorf("dry run should still COUNT what it would delete; memory_rows = %d, want 1",
			res.Deleted["memory_rows"])
	}
	// And nothing actually went.
	entries, _, err := srv.store.MemoryList(context.Background(), tenant, store.MemoryScopeUser, subject, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the dry run DELETED %d row(s); it must be inert", 1-len(entries))
	}
}

// TestErasureExecute_LiveRunRequiresMatchingConfirm locks the typo guard.
//
// A subject id that matches nothing is harmless. One that matches the WRONG
// person is irreversible, and confirm is the cheapest thing that catches it.
func TestErasureExecute_LiveRunRequiresMatchingConfirm(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	const tenant, subject = "acme", "alice"
	seedSubject(t, srv, tenant, subject)

	for _, tc := range []struct{ name, confirm string }{
		{"absent", ""},
		{"mismatched", "alicia"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := erasePost(t, srv, tenant, map[string]any{
				"subject": subject, "dry_run": false, "confirm": tc.confirm,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 — a live erasure with a %s confirm must be refused",
					rec.Code, tc.name)
			}
		})
	}
	// The data is untouched after both refusals.
	entries, _, _ := srv.store.MemoryList(context.Background(), tenant, store.MemoryScopeUser, subject, "", 0)
	if len(entries) != 1 {
		t.Errorf("a refused erasure still deleted data: %d rows remain, want 1", len(entries))
	}
}

// TestErasureExecute_ReportsResidueItIsAboutToMakeUnfindable locks the property
// that makes this operation's response irreplaceable.
//
// Tier-3 residue is reachable only by tracing provenance from the subject's chats,
// and this call deletes those chats. So the count in THIS response can never be
// recovered afterwards — see TestErasureExecute_ResidueBecomesUnfindableAfterwards
// for the other half. The erasure must therefore report the residue it is about to
// orphan, and must not delete it.
func TestErasureExecute_ReportsResidueItIsAboutToMakeUnfindable(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ctx := context.Background()
	const tenant, subject = "acme", "alice"
	seedSubject(t, srv, tenant, subject)

	rec, res := erasePost(t, srv, tenant, map[string]any{
		"subject": subject, "dry_run": false, "confirm": subject,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(res.Errors) > 0 {
		t.Fatalf("planes faulted: %v", res.Errors)
	}

	// The subject's own plane is gone.
	if res.Deleted["memory_rows"] != 1 || res.Deleted["chats"] != 1 {
		t.Errorf("deleted = %v, want memory_rows=1 chats=1", res.Deleted)
	}
	entries, _, _ := srv.store.MemoryList(ctx, tenant, store.MemoryScopeUser, subject, "", 0)
	if len(entries) != 0 {
		t.Errorf("%d user-scope row(s) survived a live erasure", len(entries))
	}

	// The residue was measured, and REPORTED, despite the chats being deleted in
	// the same call.
	if res.Residue.Rows != 1 {
		t.Errorf("residue rows = %d, want 1 — measured before the chats went, or not at all",
			res.Residue.Rows)
	}
	if _, ok := res.Retained["tier3_residue"]; !ok {
		t.Errorf("residue was counted but not named in retained: %v", res.Retained)
	}
	// The agent-scope fact is genuinely still there — the residue is real, not a
	// bookkeeping artifact.
	agentRows, _, _ := srv.store.MemoryList(ctx, tenant, store.MemoryScopeAgent, "curator", "", 0)
	if len(agentRows) != 1 {
		t.Errorf("agent-scope fact count = %d, want 1 (it must NOT be deleted)", len(agentRows))
	}
}

// TestErasureExecute_ResidueBecomesUnfindableAfterwards pins the uncomfortable
// truth, so it stays a DOCUMENTED property rather than becoming a surprise.
//
// Deleting the subject's chats destroys the only index into facts derived from
// them. A report run after an erasure therefore finds nothing to trace and would
// naturally render `residue: 0` — a false all-clear, at the exact moment someone is
// most likely to read it as confirmation the job is done.
//
// The assertion is not that the residue is gone (it is NOT — the agent-scope fact
// is still there). It is that the report refuses to present its zero as a clean
// result, and says the number is undeterminable instead.
func TestErasureExecute_ResidueBecomesUnfindableAfterwards(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	ctx := context.Background()
	const tenant, subject = "acme", "alice"
	seedSubject(t, srv, tenant, subject)
	erasePost(t, srv, tenant, map[string]any{
		"subject": subject, "dry_run": false, "confirm": subject,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/_erasure?tenant="+tenant+"&subject="+subject, nil).
		WithContext(auth.WithPrincipal(ctx, auth.Principal{
			TenantID: tenant, Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleErasureReport(rec, req)
	var rep erasureReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The fact really is still there — the residue is real, only unfindable.
	agentRows, _, _ := srv.store.MemoryList(ctx, tenant, store.MemoryScopeAgent, "curator", "", 0)
	if len(agentRows) != 1 {
		t.Fatalf("precondition: agent-scope fact count = %d, want 1", len(agentRows))
	}
	if rep.Tier3.Rows != 0 || rep.Tier3.SessionsExamined != 0 {
		t.Fatalf("precondition: expected an untraceable post-erasure state, got rows=%d sessions=%d",
			rep.Tier3.Rows, rep.Tier3.SessionsExamined)
	}

	var warned bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "UNDETERMINABLE") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a post-erasure report rendered residue 0 with no warning that it is "+
			"undeterminable — a false all-clear while the fact is still stored. notes: %v",
			rep.Notes)
	}
}

// TestErasureExecute_AlwaysReportsRetainedPlanes locks the honesty requirement.
//
// An erasure that lists only what it removed reads as complete. The usage ledger
// is kept on purpose — cost rows are accounting records — and a caller must be
// told that even when there is no tier-3 residue at all.
func TestErasureExecute_AlwaysReportsRetainedPlanes(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	_, res := erasePost(t, srv, "acme", map[string]any{"subject": "nobody-at-all"})
	if _, ok := res.Retained["usage_ledger"]; !ok {
		t.Errorf("a subject with NOTHING stored still got no retained-plane disclosure: %v",
			res.Retained)
	}
}

// TestErasureExecute_ReportsEveryPlaneItConsidered mirrors the report's invariant
// on the deletion side: a plane the operation examined must appear in `deleted`
// even when it removed nothing.
//
// Observed on the live deployment before the fix — a dry run against a real
// subject returned {"chats":2,"interrupts":0,"memory_rows":2}, with credentials,
// token_limits and path_entries silently missing because they accumulate from an
// absent key. An operator reading that cannot tell whether the subject had no
// credentials or whether credentials were never touched.
func TestErasureExecute_ReportsEveryPlaneItConsidered(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	// Deliberately a subject with NOTHING: the zero case is the one that regressed.
	_, res := erasePost(t, srv, "acme", map[string]any{"subject": "nobody-at-all"})

	for _, plane := range []string{
		"chats", "memory_rows", "path_entries",
		"credentials", "token_limits", "interrupts",
	} {
		if _, ok := res.Deleted[plane]; !ok {
			t.Errorf("plane %q missing from deleted for an empty subject — "+
				"'examined and found nothing' must not look like 'never examined'; deleted=%v",
				plane, res.Deleted)
		}
	}
}
