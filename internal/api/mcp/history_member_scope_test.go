package mcp

import (
	"context"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The MCP `history` tool is a member's chat search, as POST /v1/_history and
// the gRPC History RPC are: a member searches its own chats, [self, user],
// never the tenant's. A tenant operator adds `tenant`, an admin `global`.
func TestMcpPrincipalCtx_AMemberSearchesOnlyItsOwnChats(t *testing.T) {
	scopesOf := func(ctx context.Context) []string { return tools.PolicyOf(mcpPrincipalCtx(ctx)).History.Scopes }
	member := scopesOf(memberPrincipalCtx(t, "acme", "bob"))
	if slices.Contains(member, "tenant") || slices.Contains(member, "global") {
		t.Errorf("a member's MCP History scopes = %v, want its own chats only", member)
	}
	if !slices.Contains(member, "user") || !slices.Contains(member, "self") {
		t.Errorf("a member's MCP History scopes = %v, want [self user]", member)
	}
	op := scopesOf(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}))
	if !slices.Contains(op, "tenant") || slices.Contains(op, "global") {
		t.Errorf("a tenant operator's MCP History scopes = %v, want the tenant's chats and not global", op)
	}
	admin := scopesOf(auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}))
	if !slices.Contains(admin, "tenant") || !slices.Contains(admin, "global") {
		t.Errorf("an admin's MCP History scopes = %v, want tenant and global", admin)
	}
}
