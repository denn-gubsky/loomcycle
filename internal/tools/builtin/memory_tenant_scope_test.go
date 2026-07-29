package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// tenantMemFixture builds a Memory tool whose agent is granted the tenant scope,
// running as (tenant, user).
func tenantMemFixture(t *testing.T, tenant, user string, scopes ...string) (*Memory, context.Context, store.Store) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if len(scopes) == 0 {
		scopes = []string{"agent", "user", "tenant"}
	}
	ctx := tools.WithAgentName(context.Background(), "curator")
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a", UserID: user, TenantID: tenant})
	ctx = tools.WithMemoryPolicy(ctx, tools.MemoryPolicyValue{AllowedScopes: scopes})
	return &Memory{Store: s}, ctx, s
}

func memExecJSON(t *testing.T, m *Memory, ctx context.Context, body string) tools.Result {
	t.Helper()
	res, err := m.Execute(ctx, json.RawMessage(body))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

// TestMemoryTenantScope_SharedAcrossUsers is the scope's whole purpose: two
// different users in the SAME tenant see one keyspace.
func TestMemoryTenantScope_SharedAcrossUsers(t *testing.T) {
	m, ctxA, s := tenantMemFixture(t, "acme", "alice")
	if r := memExecJSON(t, m, ctxA, `{"op":"set","scope":"tenant","key":"house/style","value":"tabs"}`); r.IsError {
		t.Fatalf("set: %s", r.Text)
	}

	// A second user in the same tenant, same store.
	ctxB := tools.WithAgentName(context.Background(), "curator")
	ctxB = tools.WithRunIdentity(ctxB, tools.RunIdentityValue{AgentID: "b", UserID: "bob", TenantID: "acme"})
	ctxB = tools.WithMemoryPolicy(ctxB, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})
	m2 := &Memory{Store: s}

	r := memExecJSON(t, m2, ctxB, `{"op":"get","scope":"tenant","key":"house/style"}`)
	if r.IsError {
		t.Fatalf("a second user in the same tenant must read the shared row: %s", r.Text)
	}
	if !strings.Contains(r.Text, "tabs") {
		t.Errorf("want the shared value, got %q", r.Text)
	}
}

// TestMemoryTenantScope_IsolatedAcrossTenants is the property that makes it safe
// to grant at all. A shared-write scope that leaked between tenants would be the
// worst possible version of this feature.
func TestMemoryTenantScope_IsolatedAcrossTenants(t *testing.T) {
	m, ctxAcme, s := tenantMemFixture(t, "acme", "alice")
	if r := memExecJSON(t, m, ctxAcme, `{"op":"set","scope":"tenant","key":"house/style","value":"acme-only"}`); r.IsError {
		t.Fatalf("set: %s", r.Text)
	}

	ctxOther := tools.WithAgentName(context.Background(), "curator")
	ctxOther = tools.WithRunIdentity(ctxOther, tools.RunIdentityValue{AgentID: "c", UserID: "carol", TenantID: "globex"})
	ctxOther = tools.WithMemoryPolicy(ctxOther, tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}})

	r := memExecJSON(t, &Memory{Store: s}, ctxOther, `{"op":"get","scope":"tenant","key":"house/style"}`)
	if !r.IsError && strings.Contains(r.Text, "acme-only") {
		t.Fatalf("CROSS-TENANT LEAK: globex read acme's tenant-scoped row: %q", r.Text)
	}
}

// TestMemoryTenantScope_DefaultDeniedWithoutTheGrant: the per-agent allowlist IS
// the gate — there is deliberately no second mechanism, so it has to hold.
func TestMemoryTenantScope_DefaultDeniedWithoutTheGrant(t *testing.T) {
	m, ctx, _ := tenantMemFixture(t, "acme", "alice", "agent", "user")
	r := memExecJSON(t, m, ctx, `{"op":"set","scope":"tenant","key":"k","value":"v"}`)
	if !r.IsError {
		t.Fatal("an agent without the tenant grant must not reach the tenant scope")
	}
	if !strings.Contains(r.Text, "memory_scopes") {
		t.Errorf("the refusal should point at the grant: %q", r.Text)
	}
}

// TestMemoryTenantScope_DoesNotCollideWithGlobal: both use scope_id="", so the
// only thing separating them is the scope column plus the tenant partition. A
// collision here would silently merge a per-tenant keyspace into the cross-tenant
// one.
func TestMemoryTenantScope_DoesNotCollideWithGlobal(t *testing.T) {
	_, _, s := tenantMemFixture(t, "acme", "alice")
	ctx := context.Background()

	if err := s.MemorySet(ctx, "acme", store.MemoryScopeTenant, "", "shared/key", json.RawMessage(`"tenant-value"`), 0); err != nil {
		t.Fatalf("tenant set: %v", err)
	}
	if err := s.MemorySet(ctx, "", store.MemoryScopeGlobal, "", "shared/key", json.RawMessage(`"global-value"`), 0); err != nil {
		t.Fatalf("global set: %v", err)
	}

	got, err := s.MemoryGet(ctx, "acme", store.MemoryScopeTenant, "", "shared/key")
	if err != nil {
		t.Fatalf("tenant get: %v", err)
	}
	if string(got.Value) != `"tenant-value"` {
		t.Errorf("tenant row = %s, want the tenant value — global overwrote it", got.Value)
	}
	g, err := s.MemoryGet(ctx, "", store.MemoryScopeGlobal, "", "shared/key")
	if err != nil {
		t.Fatalf("global get: %v", err)
	}
	if string(g.Value) != `"global-value"` {
		t.Errorf("global row = %s, want the global value — tenant overwrote it", g.Value)
	}
}
