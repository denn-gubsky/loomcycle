package mcp

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// A workers:attach credential calls no meta-tool, the ones any authenticated
// principal may call included, whether or not its ACL set was resolved.
func TestPrincipalMayCallTool_RefusesAWorkerAttacherEveryTool(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "worker-1", TokenDefID: "def_w", Scopes: []string{auth.ScopeWorkersAttach}})
	for _, tool := range eqTools() {
		if principalMayCallTool(ctx, tool) {
			t.Errorf("a workers:attach credential may call %s", tool)
		}
	}
}
