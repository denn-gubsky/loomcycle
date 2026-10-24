package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// An auth:manage credential (RFC FA D16) reaches only HTTP user-management
// routes, none of which has an RPC twin: every RPC refuses it, including the
// ones any authenticated principal may call.
func TestEnforceScope_RefusesAnAuthManagerOnEveryRPC(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "site", Scopes: []string{auth.ScopeAuthManage}})
	for _, rpc := range serviceRPCs() {
		err := enforceScope(ctx, grpcMethodPrefix+rpc)
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: err = %v, want PermissionDenied", rpc, err)
		}
	}
}
