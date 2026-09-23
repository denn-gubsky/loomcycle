package http

import (
	"context"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func seedTenantRun(t *testing.T, st store.Store, tenant, user, agentID string) store.Run {
	t.Helper()
	sess, err := st.CreateSession(context.Background(), tenant, "a", user)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(context.Background(), sess.ID, store.RunIdentity{AgentID: agentID, UserID: user, TenantID: tenant})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

func tenantPrincipal(tenant string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant, Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
}

// MCP get_run answers from connector.GetRun, and per-tenant MCP sessions made
// it tenant-reachable. It read the raw store, so a tenant session holding
// another tenant's agent id — ids are not secret — read that run. HTTP and
// gRPC already folded the cross-tenant case into an opaque not-found.
func TestConnectorGetRun_OtherTenantsRunIsNotFound(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTenantRun(t, srv.store, "acme", "u1", "a_acme_run")

	if _, err := srv.GetRun(tenantPrincipal("other"), "a_acme_run"); err == nil {
		t.Fatal("a tenant read another tenant's run through connector.GetRun")
	} else {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			t.Errorf("cross-tenant read error = %v, want the opaque not-found", err)
		}
	}
	got, err := srv.GetRun(tenantPrincipal("acme"), "a_acme_run")
	if err != nil || got.AgentID != "a_acme_run" {
		t.Errorf("own-tenant read = (%v, %v), want the run", got.AgentID, err)
	}
}

// User ids are unique only within a tenant, so listing by user id must not
// return another tenant's runs for a colliding id.
func TestConnectorListRuns_ExcludesOtherTenantsRuns(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	seedTenantRun(t, srv.store, "acme", "shared-user", "a_acme")
	seedTenantRun(t, srv.store, "other", "shared-user", "a_other")

	runs, err := srv.ListRuns(tenantPrincipal("acme"), connector.ListRunsFilter{UserID: "shared-user"})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].AgentID != "a_acme" {
		t.Errorf("acme listed %+v, want only its own run", runs)
	}
}
