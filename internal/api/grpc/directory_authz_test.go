package grpc

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// TestDirectoryTenants_RefusesANonAdmin pins the one directory RPC that is
// cross-tenant.
//
// The counts are unremarkable; the LIST is the disclosure. A non-admin is REFUSED
// rather than handed a filtered list, because a filtered list would still confirm
// the caller's own tenant in a shape indistinguishable from "you are the only
// tenant here". The check is asserted in the handler as well as the per-RPC scope
// table — a table entry is easy to lose, and losing this one leaks which tenants
// exist.
func TestDirectoryTenants_RefusesANonAdmin(t *testing.T) {
	srv := &Server{connector: &mockConnector{}}

	tenantCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
	_, err := srv.DirectoryTenants(tenantCtx, &loomcyclepb.DirectoryTenantsRequest{})
	if err == nil {
		t.Fatal("a substrate:tenant principal enumerated tenants over gRPC")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", got)
	}
	if !strings.Contains(err.Error(), "operator-admin") {
		t.Errorf("the refusal does not say what is required: %v", err)
	}

	// The confined RPCs stay reachable for that same principal — a fix that took
	// the whole surface with it would be worse than the leak.
	if _, err := srv.DirectoryUsers(tenantCtx, &loomcyclepb.DirectoryUsersRequest{}); err != nil {
		t.Errorf("DirectoryUsers refused for a tenant principal: %v", err)
	}
	if _, err := srv.DirectoryInspect(tenantCtx,
		&loomcyclepb.DirectoryInspectRequest{Subject: "alice"}); err != nil {
		t.Errorf("DirectoryInspect refused for a tenant principal: %v", err)
	}

	// An admin may enumerate.
	adminCtx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin},
	})
	if _, err := srv.DirectoryTenants(adminCtx, &loomcyclepb.DirectoryTenantsRequest{}); err != nil {
		t.Errorf("an admin was refused DirectoryTenants: %v", err)
	}
}

// TestDirectoryInspect_RequiresASubject — an empty subject must be an
// InvalidArgument, not a zero-filled response that reads as "this person has
// nothing".
func TestDirectoryInspect_RequiresASubject(t *testing.T) {
	srv := &Server{connector: &mockConnector{}}
	_, err := srv.DirectoryInspect(context.Background(),
		&loomcyclepb.DirectoryInspectRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
	}
}

// TestDirectoryUsers_TenantComesFromThePrincipal — the request message carries no
// tenant field at all, so this asserts the RPC uses the caller's.
func TestDirectoryUsers_TenantComesFromThePrincipal(t *testing.T) {
	mc := &mockConnector{}
	srv := &Server{connector: mc}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	})
	resp, err := srv.DirectoryUsers(ctx, &loomcyclepb.DirectoryUsersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTenant() != "acme" {
		t.Errorf("response tenant = %q, want the principal's %q", resp.GetTenant(), "acme")
	}
	if got := mc.dirTenant.Load(); got == nil || got.(string) != "acme" {
		t.Errorf("connector saw tenant %v, want acme", got)
	}
}
