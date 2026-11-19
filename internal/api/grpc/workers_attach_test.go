package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A workers:attach credential attaches a worker and does nothing else (RFC GW
// Q2): every RPC of the API service refuses it, the ones any authenticated
// principal may call too.
func TestEnforceScope_RefusesAWorkerAttacherOnEveryRPC(t *testing.T) {
	for _, p := range []auth.Principal{
		{TenantID: "acme", Subject: "worker-1", Scopes: []string{auth.ScopeWorkersAttach}},
		{TenantID: "ops", Subject: "pool", Scopes: []string{auth.ScopeWorkersAttach}, Restrictions: auth.Restrictions{WorkerShared: true}},
	} {
		ctx := auth.WithPrincipal(context.Background(), p)
		for _, rpc := range serviceRPCs() {
			if err := enforceScope(ctx, grpcMethodPrefix+rpc); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s (shared=%v): err = %v, want PermissionDenied", rpc, p.Restrictions.WorkerShared, err)
			}
		}
	}
}
