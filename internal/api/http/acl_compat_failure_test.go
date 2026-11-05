package http

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// aclVersionDown is a store whose ACL rows cannot be read.
type aclVersionDown struct{ store.Store }

func (aclVersionDown) AclVersion(context.Context, string) (int64, error) {
	return 0, errors.New("store down")
}

// RFC FY Part E: when compat (the default) has to decide without the caller's
// rules because they could not be read, that is counted and audited, and the
// class baseline — static, no store read — still applies its explicit denies.
// Before, the set was dropped with a log line and an isolated user reached
// tenant-wide data the baseline denies it.
func TestACLCompat_AResolutionFailureIsAuditedAndKeepsTheIsolatedDenies(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "down.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	isolated := auth.Principal{TenantID: "acme", Subject: "iso", Scopes: []string{auth.ScopeUser}}
	tenantData := authz.Resource{Type: "memory", Name: "tenant"}

	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := makeBaseConfig()
			srv := New(cfg, &stubResolver{}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
			srv.SetACLResolver(aclres.New(aclVersionDown{st}, mode, 0))
			sink := &recordingSink{}
			srv.tokenAudit = sink
			ctx := authz.WithMode(context.Background(), mode)

			p := srv.enrichPrincipal(ctx, isolated)
			if got := srv.aclResolutionFailures.Load(); got != 1 {
				t.Errorf("failures counted = %d, want 1", got)
			}
			if len(sink.events) != 1 || sink.events[0].Action != "acl_resolution_failed" || sink.events[0].ActorSubject != "iso" {
				t.Errorf("audit = %+v, want one acl_resolution_failed for iso", sink.events)
			}
			r := authz.Evaluate(ctx, p.Effective, "read", tenantData, true)
			if r.Allowed {
				t.Errorf("an isolated user read tenant memory while its rules were unreadable: %+v", r)
			}
			if mode == authz.ModeEnforce && p.Effective != nil {
				t.Error("enforce mode kept a set it could not read; it must refuse instead")
			}
			rec := httptest.NewRecorder()
			srv.handleMetricsProm(rec, httptest.NewRequest("GET", "/metrics", nil))
			if !strings.Contains(rec.Body.String(), "loomcycle_acl_resolution_failures_total") {
				t.Error("the counter is not exported on /metrics")
			}
		})
	}
}

// The simulator compiles the simulated subject's own class baseline: an
// isolated user's draft is judged with the isolated baseline's denies, not a
// member's grants.
func TestACLSimulate_UsesTheSubjectsClassBaseline(t *testing.T) {
	_, ts, st := aclMgmtServer(t, authz.ModeCompat)
	if err := st.UserCreate(context.Background(), store.UserRow{TenantID: "acme", Subject: "iso", AccessMode: "isolated", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	body := `{"subject":{"id":"iso"},"acls":[{"name":"draft","rules":[{"effect":"allow","actions":["read"],"resources":["memory:*"]}]}],` +
		`"bindings":[{"acl":"draft","subject_kind":"user","subject_id":"iso"}],` +
		`"requests":[{"action":{"name":"read"},"resource":{"type":"memory","id":"tenant"}}]}`
	code, out := bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/simulate", body)
	if code != 200 {
		t.Fatalf("simulate: %d %s", code, out)
	}
	if !strings.Contains(out, `"with_draft":{"decision":false,"outcome":"deny"`) {
		t.Errorf("the draft was judged without the isolated baseline's deny: %s", out)
	}
}
