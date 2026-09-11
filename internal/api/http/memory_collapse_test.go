package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The collapse gives a fact ONE home (RFC CV P2f). These pin the refusals, because
// the refusals are the feature: a migration that deletes is only as good as what
// stops it.

// withSqlMem gives the fixture a real SQL Memory manager, so the chunk plane
// EXISTS and is merely empty — which is the state these refusals are about.
func withSqlMem(t *testing.T, srv *Server) {
	t.Helper()
	mgr, err := sqlmem.New(sqlmem.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("sqlmem.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	srv.sqlMem = mgr
}

func collapseReq(t *testing.T, srv *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/collapse_facts"+query, nil).
		WithContext(auth.WithPrincipal(context.Background(), auth.Principal{
			Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}))
	srv.handleMemoryCollapseFacts(rec, req)
	return rec
}

// TestCollapseFacts_RefusesWhenSqlMemIsAbsent. Without SQL Memory there is no
// chunk plane, so EVERY fact would read as unmirrored. Saying "not configured"
// is the difference between a missing capability and a corrupt store — the same
// distinction the backfill endpoint makes for a tier without vectors.
func TestCollapseFacts_RefusesWhenSqlMemIsAbsent(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	srv.sqlMem = nil

	rec := collapseReq(t, srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with no chunk plane; body: %s", rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "sqlmem_unavailable" {
		t.Errorf("error = %v, want sqlmem_unavailable — an operator must not read this as "+
			"'your facts have no chunks'", e["code"])
	}
}

// TestCollapseFacts_AnAdminMustNameTheTenant. Memory rows are keyed on the tenant,
// so an admin naming none resolves to the DEFAULT tenant — and this DELETES. The
// same refusal the stale-embedding purge makes, for the same reason.
func TestCollapseFacts_AnAdminMustNameTheTenant(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	rec := collapseReq(t, srv, "?scope=user&scope_id=alice&dry_run=false") // no ?tenant=
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when an admin names no tenant; body: %s", rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "tenant_required" {
		t.Errorf("error = %v, want tenant_required — a destructive sweep must not resolve "+
			"silently to the default tenant", e["code"])
	}
}

// TestCollapseFacts_RefusesTheWHOLEScopeOnOneUnmirroredFact is the safety property.
//
// A fact with no chunk has nowhere to go, so deleting its k/v row deletes the
// fact. A PARTIAL collapse would be the worst outcome: the operator sees a success
// while the un-homed facts sit one run away from deletion. So one unmirrored row
// stops the scope, and the response names them.
func TestCollapseFacts_RefusesTheWHOLEScopeOnOneUnmirroredFact(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	ctx := context.Background()
	// Two facts, no chunk plane content at all — so both are unmirrored.
	for _, k := range []string{"memory/fact/a", "memory/fact/b"} {
		if err := srv.store.MemorySet(ctx, "", store.MemoryScopeUser, "alice", k,
			[]byte(`"a fact"`), 0); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	rec := collapseReq(t, srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 when a fact has no chunk; body: %s", rec.Code, rec.Body.String())
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e["code"] != "unmirrored_facts" {
		t.Errorf("error = %v, want unmirrored_facts", e["code"])
	}
	// And nothing was deleted — the refusal must be total.
	rows, _, err := srv.store.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "memory/", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("%d fact rows survive the refusal, want 2 — a refused collapse must delete nothing", len(rows))
	}
}

// TestCollapseFacts_DryRunIsTheDefaultAndDeletesNothing. A bare POST must never
// remove a row: dry_run defaults TRUE, as it does on the stale-embedding purge.
func TestCollapseFacts_DryRunIsTheDefaultAndDeletesNothing(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	ctx := context.Background()
	// A DOCUMENT body, which the collapse must never consider at all.
	if err := srv.store.MemorySet(ctx, "", store.MemoryScopeUser, "alice",
		"doc.chunk:deadbeef", []byte(`{"body":"prose"}`), 0); err != nil {
		t.Fatalf("seed prose: %v", err)
	}
	rec := collapseReq(t, srv, "?scope=user&scope_id=alice&tenant=") // no dry_run param
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var rep memoryCollapseReport
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if !rep.DryRun {
		t.Error("dry_run defaulted to FALSE — a bare POST would delete fact rows")
	}
	if rep.Facts != 0 {
		t.Errorf("counted %d facts in a scope holding only a document body — the collapse "+
			"must look at memory/ rows ONLY, or it would delete the document store", rep.Facts)
	}
	rows, _, err := srv.store.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "doc.chunk:", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("the document body did not survive: %d rows", len(rows))
	}
}

// TestCollapseFacts_DeletesTheFactRowAndNothingElse is the happy path, and the
// assertion that matters most is the negative one: a document BODY lives in the
// same keyspace at doc.chunk:<hex> and is the thing a fact is being moved INTO.
// Deleting one would destroy the document store, so the boundary is asserted
// rather than assumed.
func TestCollapseFacts_DeletesTheFactRowAndNothingElse(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	withSqlMem(t, srv)
	ctx := context.Background()

	// The chunk plane, written the ordinary way so the fact genuinely has a home.
	doc := &builtin.Document{Store: srv.store, SqlMem: srv.sqlMem}
	dctx := tools.WithAgentName(tools.WithRunIdentity(ctx, tools.RunIdentityValue{UserID: "alice"}), "collapser")
	mk := func(body string) tools.Result {
		res, err := doc.Execute(dctx, json.RawMessage(body))
		if err != nil {
			t.Fatalf("document: %v", err)
		}
		return res
	}
	created := mk(`{"op":"create_document","scope":"user","title":"entities","path":"/t/e"}`)
	var cd struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal([]byte(created.Text), &cd)
	if r := mk(`{"op":"upsert_chunk","scope":"user","document_id":"` + cd.DocumentID + `",
		"natural_key":"memory/fact/mirrored","title":"A fact","body":"A mirrored fact.",
		"type":"fact","subject":"X"}`); r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}

	// The k/v twin the collapse deletes, with retrieval counters to carry.
	if err := srv.store.MemorySet(ctx, "", store.MemoryScopeUser, "alice",
		"memory/fact/mirrored", []byte(`"A mirrored fact."`), 0); err != nil {
		t.Fatalf("seed fact: %v", err)
	}
	if err := srv.store.MemoryBumpAccessBatch(ctx, []store.MemoryAccessBump{{
		Scope: store.MemoryScopeUser, ScopeID: "alice", Key: "memory/fact/mirrored", CountDelta: 7,
	}}); err != nil {
		t.Fatalf("seed access: %v", err)
	}

	rec := collapseReq(t, srv, "?scope=user&scope_id=alice&tenant=&dry_run=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var rep memoryCollapseReport
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	if rep.Deleted != 1 || rep.Mirrored != 1 || rep.Unmirrored != 0 {
		t.Errorf("report = %+v, want one mirrored fact deleted", rep)
	}
	if rep.Copied != 1 {
		t.Errorf("access counts copied = %d, want 1 — the counter feeds the recall ranker's "+
			"frequency term and lives on the row being deleted", rep.Copied)
	}

	// The fact row is gone...
	facts, _, err := srv.store.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "memory/", 100)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("%d fact rows survive the collapse, want 0", len(facts))
	}
	// ...and the chunk BODY — the fact's new home, and every document's home — is not.
	bodies, _, err := srv.store.MemoryList(ctx, "", store.MemoryScopeUser, "alice", "doc.chunk:", 100)
	if err != nil {
		t.Fatalf("list bodies: %v", err)
	}
	if len(bodies) == 0 {
		t.Fatal("the collapse deleted the chunk bodies — it must touch memory/ rows ONLY, " +
			"or it destroys the document store along with the duplication")
	}
	// The carried counter landed on the surviving row.
	var carried int64
	for _, b := range bodies {
		if b.AccessCount > carried {
			carried = b.AccessCount
		}
	}
	if carried != 7 {
		t.Errorf("the surviving row carries access_count=%d, want 7 — an uncopied counter "+
			"resets every fact's frequency term and silently shifts ranking", carried)
	}
}
