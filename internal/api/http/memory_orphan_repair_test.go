package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedLegacyMemory writes a row into the legacy "" tenant partition — where every
// row written before RFC BL added tenant_id still sits.
func seedLegacyMemory(t *testing.T, srv *Server, scope store.MemoryScope, scopeID, key string) {
	t.Helper()
	if err := srv.store.MemorySet(context.Background(), "", scope, scopeID, key, json.RawMessage(`"v"`), 0); err != nil {
		t.Fatalf("seed legacy %s/%s: %v", scopeID, key, err)
	}
}

func decodeRepair(t *testing.T, rec *httptest.ResponseRecorder) memoryOrphanRepairResponse {
	t.Helper()
	var out memoryOrphanRepairResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

// TestMemoryOrphanRepair_DryRunIsTheDefault: a POST that omits dry_run must
// REPORT and not write. This is a bulk row rewrite, so an ambiguous request has
// to resolve to the read-only reading — an operator poking the endpoint from the
// Settings hub should never mutate rows by accident.
func TestMemoryOrphanRepair_DryRunIsTheDefault(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	seedLegacyMemory(t, srv, store.MemoryScopeUser, "u1", "doc.chunk:a")
	adminCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "tnt", Subject: "root", Scopes: []string{auth.ScopeAdmin}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/repair-tenant", strings.NewReader(`{}`)).WithContext(adminCtx)
	srv.handleMemoryOrphanRepair(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeRepair(t, rec)
	if got.Applied {
		t.Error("Applied = true for a request that omitted dry_run; the default must not write")
	}
	if got.Orphaned != 1 {
		t.Errorf("Orphaned = %d, want 1", got.Orphaned)
	}
	// Tenant defaulted to the caller's own, and is echoed so a caller that
	// relied on the default can see what was acted on.
	if got.Tenant != "tnt" {
		t.Errorf("Tenant = %q, want %q (the caller's own)", got.Tenant, "tnt")
	}
	// The row must still be at the legacy partition.
	if _, err := srv.store.MemoryGet(context.Background(), "", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("dry run moved the row: %v", err)
	}
}

// TestMemoryOrphanRepair_AppliesWhenAsked: dry_run:false commits, and the row
// becomes readable at the target tenant.
func TestMemoryOrphanRepair_AppliesWhenAsked(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	seedLegacyMemory(t, srv, store.MemoryScopeUser, "u1", "doc.chunk:a")
	seedLegacyMemory(t, srv, store.MemoryScopeGlobal, "__help__", "topic#x")
	adminCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "tnt", Subject: "root", Scopes: []string{auth.ScopeAdmin}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/repair-tenant",
		strings.NewReader(`{"tenant":"tnt","dry_run":false}`)).WithContext(adminCtx)
	srv.handleMemoryOrphanRepair(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decodeRepair(t, rec)
	if !got.Applied || got.Moved != 1 {
		t.Errorf("Applied=%v Moved=%d, want true/1 (body=%s)", got.Applied, got.Moved, rec.Body.String())
	}
	if _, err := srv.store.MemoryGet(context.Background(), "tnt", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("row not readable at the target tenant after repair: %v", err)
	}
	// The global row is shared by design and must be left at "".
	if _, err := srv.store.MemoryGet(context.Background(), "", store.MemoryScopeGlobal, "__help__", "topic#x"); err != nil {
		t.Errorf("global row was moved off the legacy partition: %v", err)
	}
	if got.SkippedGlobal != 1 {
		t.Errorf("SkippedGlobal = %d, want 1", got.SkippedGlobal)
	}
}

// TestMemoryOrphanRepair_TenantRequiredWithoutOne: a legacy/open-mode admin token
// carries no tenant, and "" is the legacy partition rather than a repair target —
// so the server must refuse instead of guessing which tenant the rows belong to.
func TestMemoryOrphanRepair_TenantRequiredWithoutOne(t *testing.T) {
	srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
	seedLegacyMemory(t, srv, store.MemoryScopeUser, "u1", "doc.chunk:a")
	// Admin principal with NO tenant of its own.
	adminCtx := auth.WithPrincipal(context.Background(),
		auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/_memory/repair-tenant", strings.NewReader(`{"dry_run":false}`)).WithContext(adminCtx)
	srv.handleMemoryOrphanRepair(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tenant_required") {
		t.Errorf("body = %s, want code tenant_required", rec.Body.String())
	}
	if _, err := srv.store.MemoryGet(context.Background(), "", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("a refused request still moved the row: %v", err)
	}
}

// TestMemoryOrphanRepair_RouteIsAdminOnly pins the scope gate: the endpoint
// rewrites rows across every scope in one statement, which is not a tenant
// operator's authority even over its own tenant. It is admin via the /v1/_*
// catch-all rather than an explicit case, so assert the resolved scope directly —
// a future explicit case that downgraded it would otherwise pass unnoticed.
func TestMemoryOrphanRepair_RouteIsAdminOnly(t *testing.T) {
	if got := requiredScopeFor(http.MethodPost, "/v1/_memory/repair-tenant"); got != auth.ScopeAdmin {
		t.Errorf("requiredScopeFor = %q, want %q", got, auth.ScopeAdmin)
	}
}
