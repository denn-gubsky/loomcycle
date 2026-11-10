package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A metrics:read credential reaches only GET /metrics over HTTP: every RPC
// refuses it, the ones any authenticated principal may call too.
func TestEnforceScope_RefusesAMetricsReaderOnEveryRPC(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "prometheus", Scopes: []string{auth.ScopeMetricsRead}})
	for _, rpc := range serviceRPCs() {
		if err := enforceScope(ctx, grpcMethodPrefix+rpc); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: err = %v, want PermissionDenied", rpc, err)
		}
	}
}
