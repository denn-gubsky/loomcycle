package credential

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

func newEngine(t *testing.T, kek string) *Engine {
	t.Helper()
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := NewSealer(kek, "")
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return NewEngine(st, sealer)
}

func TestEngine_PutResolveRoundTrip(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	id := Identity{TenantID: "t", Scope: "tenant", Name: "serper"}

	meta, err := e.PutInline(ctx, id, "sk-secret", nil)
	if err != nil {
		t.Fatalf("PutInline: %v", err)
	}
	if meta.Definition != nil {
		t.Error("PutInline returned the sealed definition; must be metadata-only")
	}
	if meta.Backend != "inline" {
		t.Errorf("backend = %q, want inline", meta.Backend)
	}

	res, found, err := e.Resolve(ctx, "t", "", "", "serper")
	if err != nil || !found {
		t.Fatalf("Resolve: found=%v err=%v", found, err)
	}
	if res.Value != "sk-secret" || res.Scope != "tenant" {
		t.Errorf("Resolve = %+v, want sk-secret@tenant", res)
	}

	if got, _ := e.Get(ctx, id); got.Definition != nil {
		t.Error("Get leaked the sealed definition; must be metadata-only")
	}
}

func TestEngine_ResolvePrecedence(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	put := func(scope, scopeID, val string) {
		if _, err := e.PutInline(ctx, Identity{TenantID: "t", Scope: scope, ScopeID: scopeID, Name: "telegram"}, val, nil); err != nil {
			t.Fatalf("put %s/%s: %v", scope, scopeID, err)
		}
	}
	put("tenant", "", "team-token")
	put("user", "userA", "A-token")
	put("agent", "agent-x", "agent-token")

	// user A shadows the tenant default.
	if res, found, _ := e.Resolve(ctx, "t", "", "userA", "telegram"); !found || res.Value != "A-token" || res.Scope != "user" {
		t.Errorf("userA resolve = %+v (found=%v), want A-token@user", res, found)
	}
	// user B (no override) falls back to the tenant default.
	if res, found, _ := e.Resolve(ctx, "t", "", "userB", "telegram"); !found || res.Value != "team-token" || res.Scope != "tenant" {
		t.Errorf("userB resolve = %+v, want team-token@tenant", res)
	}
	// agent scope is the most specific — wins over user + tenant.
	if res, found, _ := e.Resolve(ctx, "t", "agent-x", "userA", "telegram"); !found || res.Value != "agent-token" || res.Scope != "agent" {
		t.Errorf("agent resolve = %+v, want agent-token@agent", res)
	}
	// no user/agent id → tenant default.
	if res, found, _ := e.Resolve(ctx, "t", "", "", "telegram"); !found || res.Value != "team-token" {
		t.Errorf("bare resolve = %+v, want team-token", res)
	}
}

func TestEngine_MissAndFailClosed(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, testKey(1))
	if _, found, err := e.Resolve(ctx, "t", "", "", "absent"); err != nil || found {
		t.Errorf("resolve of absent = (found=%v, err=%v), want (false, nil)", found, err)
	}

	disabled := newEngine(t, "") // no KEK
	if disabled.InlineEnabled() {
		t.Error("engine with no KEK should report InlineEnabled()==false")
	}
	if _, err := disabled.PutInline(ctx, Identity{TenantID: "t", Scope: "tenant", Name: "x"}, "v", nil); err == nil {
		t.Error("PutInline with no KEK must fail (never store plaintext)")
	}
}

func TestEngine_ResolveIsTenantIsolated(t *testing.T) {
	e := newEngine(t, testKey(1))
	ctx := context.Background()
	if _, err := e.PutInline(ctx, Identity{TenantID: "tnt-a", Scope: "tenant", Name: "serper"}, "a-secret", nil); err != nil {
		t.Fatal(err)
	}
	// A different tenant resolving the same name gets nothing.
	if _, found, _ := e.Resolve(ctx, "tnt-b", "", "", "serper"); found {
		t.Error("tenant B resolved tenant A's credential — isolation breach")
	}
}
