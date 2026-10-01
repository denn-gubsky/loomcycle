package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// In open mode (no bearer configured) the auth middleware stamps no principal:
// whoever calls /v1/_* is the operator. The def tools must treat that call as
// operator authority — here, a trigger whose runs execute in a named tenant,
// which only an admin may author. The middleware is what makes "no principal"
// mean "open mode" on this surface, so substrateAdminCtx is the one place that
// may say so.
//
// Fails-before: 422 — the open-mode operator read as a tenant-less non-admin.
func TestSubstrateAdmin_OpenModeOperatorMayAuthorForeignExecTenant(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100}}
	srv := New(cfg, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)
	srv.SetScheduleDefTool(&builtin.ScheduleDef{Store: st, Cfg: cfg})
	server := httptest.NewServer(srv.Mux())
	t.Cleanup(server.Close)
	ts := server.URL

	body := `{"op":"create","name":"acme-digest","overlay":{"agent":"researcher","schedule":"0 6 * * *","tenant_id":"acme"}}`
	resp, err := http.Post(ts+"/v1/_scheduledef", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 — an open-mode operator may point a trigger at any tenant; body=%s", resp.StatusCode, raw)
	}
	row, err := st.ScheduleDefGetActive(context.Background(), "", "acme-digest")
	if err != nil {
		t.Fatalf("get active: %v", err)
	}
	var def struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.Unmarshal(row.Definition, &def); err != nil || def.TenantID != "acme" {
		t.Errorf("stored exec tenant %q (err %v), want acme", def.TenantID, err)
	}
}

// The marker is stamped only where no principal exists: an authenticated
// off-run call keeps its principal's authority and gains nothing.
func TestSubstrateAdminCtx_MarksOnlyTheUnauthenticatedOperator(t *testing.T) {
	if !tools.IsUnauthenticatedOperator(substrateAdminCtx(context.Background())) {
		t.Error("the no-principal (open-mode) substrate ctx does not carry the unauthenticated-operator marker")
	}
	withP := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
	if tools.IsUnauthenticatedOperator(substrateAdminCtx(withP)) {
		t.Error("an authenticated substrate ctx carries the unauthenticated-operator marker")
	}
}
