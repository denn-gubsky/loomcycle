package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A scim:provision credential (RFC FA P6) reaches only /scim/v2 over HTTP:
// every RPC refuses it, the ones any authenticated principal may call too.
func TestEnforceScope_RefusesASCIMProvisionerOnEveryRPC(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "idp", Scopes: []string{auth.ScopeSCIMProvision}})
	for _, rpc := range serviceRPCs() {
		if err := enforceScope(ctx, grpcMethodPrefix+rpc); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: err = %v, want PermissionDenied", rpc, err)
		}
	}
}
