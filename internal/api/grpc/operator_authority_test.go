package grpc

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Open-mode gRPC stamps no principal, so the substrate ctx must say "this is
// the operator" for the def tools (mirrors HTTP substrateAdminCtx and the MCP
// operatorCtx). An authenticated call keeps its principal's authority only.
//
// Fails-before: the no-principal ctx carried no marker.
func TestSubstrateGRPCCtx_MarksOnlyTheUnauthenticatedOperator(t *testing.T) {
	if !tools.IsUnauthenticatedOperator(substrateGRPCCtx(context.Background())) {
		t.Error("the no-principal (open-mode) gRPC substrate ctx does not carry the unauthenticated-operator marker")
	}
	withP := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
	if tools.IsUnauthenticatedOperator(substrateGRPCCtx(withP)) {
		t.Error("an authenticated gRPC substrate ctx carries the unauthenticated-operator marker")
	}
}
