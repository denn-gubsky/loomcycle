package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The crossing, not the ends.
//
// tools.EffectiveMemoryScopes is unit-tested, and the Memory tool's scope gate
// is unit-tested, and BOTH pass while the resolver's value never reaches the
// gate — because the policy is threaded through ctx by a dozen construction
// sites rather than called for. This drives the seam: resolve as a run-start
// site does, attach as it does, then ask the tool to resolve a scope.
func TestMemory_DefaultedScopeIsAcceptedByTheTool(t *testing.T) {
	m := &Memory{}

	runStartCtx := func(id tools.RunIdentityValue, declared []string) context.Context {
		ctx := tools.WithRunIdentity(context.Background(), id)
		// Exactly what a run-start site does now.
		return tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{
			AllowedScopes: tools.EffectiveMemoryScopes(ctx, declared),
		})
	}

	t.Run("an agent with NO memory_scopes can now use the user scope", func(t *testing.T) {
		ctx := runStartCtx(tools.RunIdentityValue{UserID: "u1", TenantID: "acme"}, nil)
		scope, scopeID, err := m.resolveScope(ctx, "user")
		if err != nil {
			t.Fatalf("user scope refused for a defaulted agent: %v", err)
		}
		if string(scope) != "user" || scopeID != "u1" {
			t.Errorf("scope=%q scope_id=%q, want user/u1 — the default resolved but did not "+
				"carry the run's identity into the scope_id", scope, scopeID)
		}
	})

	t.Run("the tenant half is refused for an isolated member", func(t *testing.T) {
		ctx := runStartCtx(tools.RunIdentityValue{UserID: "u1", TenantID: "acme", Isolated: true}, nil)
		if _, _, err := m.resolveScope(ctx, "tenant"); err == nil {
			t.Error("an isolated run resolved the tenant scope; the default must not offer it " +
				"and ConfineIsolatedScope must refuse it even if it did")
		}
	})

	t.Run("an explicit deny still refuses, and says how to fix it", func(t *testing.T) {
		ctx := runStartCtx(tools.RunIdentityValue{UserID: "u1"}, []string{tools.DenyAllScopes})
		_, _, err := m.resolveScope(ctx, "user")
		if err == nil {
			t.Fatal("deny-all did not refuse — an operator can no longer say no")
		}
		if !strings.Contains(err.Error(), "memory_scopes") {
			t.Errorf("refusal %q does not name the field to change", err)
		}
	})

	t.Run("a declared list is still authoritative and is not widened", func(t *testing.T) {
		ctx := runStartCtx(tools.RunIdentityValue{UserID: "u1", TenantID: "acme"}, []string{"agent"})
		if _, _, err := m.resolveScope(ctx, "user"); err == nil {
			t.Error("memory_scopes: [agent] resolved the USER scope — the default widened an " +
				"explicit grant, which it must never do")
		}
	})
}
