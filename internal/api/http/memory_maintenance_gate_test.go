package http

import (
	"net/http"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// The memory maintenance routes rewrite a scope's rows in bulk — re-embed,
// backfill, purge, collapse, re-home — and several spend the operator's
// embedder or model budget. They are an operator's action: a tenant operator
// and an admin reach them, a tenant member does not.
var memoryMaintenanceRoutes = []string{
	"/v1/_memory/reembed",
	"/v1/_memory/backfill_embeddings",
	"/v1/_memory/backfill_traces",
	"/v1/_memory/purge_stale_embeddings",
	"/v1/_memory/collapse_facts",
	"/v1/_memory/home_facts",
}

func TestMemoryMaintenanceRoutes_MemberRefusedTenantOperatorAdmitted(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedToken(t, st, "lct_member", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	seedToken(t, st, "lct_tenant", "acme", "op", []string{auth.ScopeTenant}, time.Time{})
	seedToken(t, st, "lct_admin", "acme", "root", []string{auth.ScopeAdmin}, time.Time{})
	for _, path := range memoryMaintenanceRoutes {
		if reached, code := runMW(t, s, http.MethodPost, path, "lct_member"); reached || code != http.StatusForbidden {
			t.Errorf("member POST %s: code=%d reached=%v, want 403 and not reached", path, code, reached)
		}
		for _, tok := range []string{"lct_tenant", "lct_admin"} {
			if reached, code := runMW(t, s, http.MethodPost, path, tok); !reached || code == http.StatusForbidden {
				t.Errorf("%s POST %s: code=%d reached=%v, want admitted", tok, path, code, reached)
			}
		}
		if tenantMemberAccessible(http.MethodPost, path) {
			t.Errorf("tenantMemberAccessible(POST %s) = true, want false", path)
		}
	}
	// The member's own memory surfaces stay open.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/_memory/scopes"}, {http.MethodPost, "/v1/_memory/search"},
		{http.MethodGet, "/v1/_memory/embed_stats"},
		{http.MethodGet, "/v1/_memory/scopes/user/alice/keys/k"},
	} {
		if reached, code := runMW(t, s, c.method, c.path, "lct_member"); !reached || code == http.StatusForbidden {
			t.Errorf("member %s %s: code=%d reached=%v, want admitted", c.method, c.path, code, reached)
		}
	}
}
